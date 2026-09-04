# sub2cc-quota-scheduler

A tiny static Go binary that makes [sub2api](https://github.com/Wei-Shaw/sub2api)
spend Claude subscription quota **before it expires**. It reorders the accounts
of one sub2api group every five minutes based on each account's 7-day usage
window and reset countdown, and it can reserve part of one account for your
own use. It talks only to the sub2api admin API, so sub2api itself, its
database, and its service are never modified.

- Standard library only, single ~6 MB static binary, ~3 MB RSS, ~30 ms per run.
- Runs from a systemd timer next to sub2api; nothing to install on the host.
- Shadow mode first: logs what it *would* change until you flip it to apply.
- Never moves an existing sticky session by reordering; only the optional
  hard reserve can.

## Motivation

Claude subscriptions (Max, Team, and similar) meter usage in a rolling
**7-day window** that resets at a fixed time. Whatever you have not used when
the window resets is gone. If you pool several subscriptions behind sub2api,
the interesting question is therefore not "which account is cheapest" but
"which account's unused capacity is about to vanish".

sub2api v0.2.0 cannot answer that question on its own:

- New sessions are assigned by a **static per-account priority**, then load,
  then least-recently-used. The 7-day reset time is not a selection key.
- Its optional `prefer_soonest_reset` looks only at the **5-hour** window.
- It does record the Anthropic `7d` and `7d_oi` usage headers for every
  account, and its threshold feature can pause an account above a percentage,
  but that threshold is one number shared by all windows and all accounts.
- Sticky sessions (one hour, refreshed on use) are honored before priority,
  which is exactly what you want: a running coding session should not hop
  between accounts and lose its prompt cache.

The upstream feature request for reset-aware scheduling
([Wei-Shaw/sub2api#5583](https://github.com/Wei-Shaw/sub2api/issues/5583))
was open with no response at the time of writing. Forking sub2api to add an
in-process scheduler means rebuilding and re-patching on every upstream
release. This project takes the other route: a sidecar that uses the data
sub2api already collects and the admin API it already exposes.

Two concrete needs drove the design:

1. **Do not waste expiring capacity.** An account whose 7-day window resets
   in a day and still has 70% unused should receive new sessions before an
   account that resets next week, even if the operator's normal preference
   says otherwise.
2. **Keep a private reserve.** One subscription is also used for the owner's
   own chat. It must never be driven past a chosen usage level by gateway
   traffic, with a separate, higher level for models that have their own
   sub-window (the Anthropic `7d_oi` window used by Fable models).

## How it works

Every run (default: every five minutes):

1. Fetch the group's accounts and the group's model routing through the admin
   API. Abort without writing if any configured account is missing or not in
   the group.
2. Normalize each subscription's 7-day windows from the passive usage fields
   sub2api stores in `extra`:
   - `known`: utilization and a future reset timestamp.
   - `rolled`: the recorded reset is in the past, so the window has rolled
     over and no new sample has arrived yet; usage is treated as 0 and the
     reset is advanced by whole weeks.
   - `unknown`: fields missing (sub2api clears them at each new 5-hour
     window until the next response is sampled); the account falls back to
     the base order and never triggers a reserve action.
3. Build the **urgent tier**: subscriptions whose reset is within
   `lookahead_hours` and whose headroom below their ceiling is at least
   `min_urgent_headroom_percent`. Rank them by
   `pressure = headroom / hours_to_reset`, so "a lot left, little time" wins.
   Two urgent accounts keep their previous relative order unless the higher
   pressure exceeds the lower by more than `hysteresis_ratio`, which prevents
   flapping.
4. Target order = urgent tier, then every other account in the configured
   base order. Positions become priorities `1..N`. Only accounts whose live
   priority differs are written, with a request body containing nothing but
   `priority`.
5. For accounts marked `enforce_ceiling`:
   - 7-day usage at or above `ceiling_percent`: set the account unschedulable
     until its window resets, then re-enable it. The scheduler remembers that
     it was the one who disabled the account and never re-enables an account
     an operator disabled by hand.
   - Fable (`7d_oi`) usage at or above `fable_ceiling_percent`: install a
     group model routing rule that sends `fable_model_pattern` to the other
     configured accounts, and remove it after the reset. If the group already
     has routing the scheduler did not write, it leaves it alone and warns.
6. Print one JSON decision line, append it to `decisions.jsonl`, and persist
   the small state file.

Everything that is *not* the reserve is soft: it only changes where **new**
sessions land. sub2api's own sticky-session logic keeps existing sessions on
their account, and its own rate-limit and threshold handling still applies on
top.

A worked example from a live deployment (five accounts, base order
`relay > sub-a > sub-b > team-plan > personal-max`):

| Account | 7d used | Resets in | Headroom | Pressure | Result |
| --- | ---: | ---: | ---: | ---: | --- |
| team-plan | 25% | 21 h | 70 | 3.36 | urgent, priority 1 |
| personal-max (ceiling 60) | 35% | 72 h | 25 | 0.35 | urgent, priority 2 |
| relay | n/a | n/a | | | base, priority 3 |
| sub-a | 24% | 159 h | | | base, priority 4 |
| sub-b | 33% | 123 h | | | base, priority 5 |

## Quick start

Requirements: Go 1.22+ on your workstation to build; sub2api v0.2.0 or
compatible on the target host; nothing else on the host.

1. **Build** a static `linux/amd64` binary:

   ```sh
   sh build.sh          # writes dist/sub2cc-quota-scheduler
   ```

2. **Create an admin API key** in the sub2api admin UI (Settings, admin API
   key, regenerate). Optionally set the platform scheduling threshold there
   as a backstop for accounts without an explicit reserve.

3. **Write the config.** Copy `deploy/config.example.json` to `config.json`
   and set `group_id`, the account IDs in your preferred base order, and any
   per-account ceilings. Leave `mode` as `shadow`.

4. **Install on the host.** Put `dist/sub2cc-quota-scheduler`, `config.json`,
   `deploy/sub2cc-quota-scheduler.service`, `deploy/sub2cc-quota-scheduler.timer`,
   and `deploy/install.sh` in one directory on the host, then:

   ```sh
   sudo sh install.sh
   sudo sh -c 'umask 077; echo "SUB2CC_SCHEDULER_ADMIN_KEY=admin-..." > /etc/sub2cc-scheduler/env'
   sudo systemctl start sub2cc-quota-scheduler.service
   journalctl -u sub2cc-quota-scheduler -n 3 -o cat
   ```

   The installer copies the binary to `/opt/sub2cc-scheduler`, the config to
   `/etc/sub2cc-scheduler/config.json`, and enables the timer. The service
   runs as a dynamic unprivileged user, may only reach `127.0.0.1`, and keeps
   its state in `/var/lib/sub2cc-scheduler`.

5. **Watch shadow decisions** for a while:

   ```sh
   tail -n 1 /var/lib/sub2cc-scheduler/decisions.jsonl | python3 -m json.tool
   ```

   Each line lists every account with its windows, headroom, pressure, and
   target priority, plus the `actions` it would take and any `warnings`.

6. **Enable writes** by changing `"mode": "shadow"` to `"mode": "apply"` in
   `/etc/sub2cc-scheduler/config.json`. The next run applies the actions; a
   run with nothing to change performs no writes.

Rollback at any time: `sudo systemctl disable --now sub2cc-quota-scheduler.timer`.
Priorities, the schedulable flag, and group routing remain ordinary fields
you can edit in the sub2api admin UI.

## Configuration reference

| Key | Default | Meaning |
| --- | --- | --- |
| `base_url` | `http://127.0.0.1:8080` | sub2api address; keep it on loopback |
| `admin_key_env` | `SUB2CC_SCHEDULER_ADMIN_KEY` | environment variable that holds the admin API key |
| `mode` | `shadow` | `shadow` logs only; `apply` writes |
| `group_id` | required | the sub2api group whose accounts are managed |
| `lookahead_hours` | `72` | how early an expiring window becomes urgent |
| `min_urgent_headroom_percent` | `5` | minimum unused percent worth promoting for |
| `hysteresis_ratio` | `0.2` | relative pressure gap needed to reorder two urgent accounts |
| `default_ceiling_percent` | `95` | 7d ceiling used for headroom when an account sets none |
| `default_fable_ceiling_percent` | `95` | `7d_oi` ceiling used when an account sets none |
| `fable_model_pattern` | `claude-fable-*` | routing pattern installed for the Fable reserve |
| `accounts[]` | required | ordered list; each has `id`, `name`, `kind` (`relay` or `subscription`), optional `ceiling_percent`, `fable_ceiling_percent`, `enforce_ceiling` |

`relay` accounts (API-key relays with no visible quota) always stay in the
base order. Only the listed accounts are ever read for policy or written.

## Commands

```sh
sub2cc-quota-scheduler run  --config /etc/sub2cc-scheduler/config.json
sub2cc-quota-scheduler plan --config /etc/sub2cc-scheduler/config.json --state-dir /tmp/qs --now 2026-09-05T05:30:00Z
sub2cc-quota-scheduler version
```

`plan` never writes, whatever the config says, and accepts `--now` to preview
a future point in time. `--state-dir` defaults to systemd's
`$STATE_DIRECTORY`.

## Safety properties

- Reads: `GET /api/v1/admin/accounts?group=<id>` and `GET /api/v1/admin/groups/<id>`.
- Writes, apply mode only: `PUT /api/v1/admin/accounts/<id>` with `{"priority": n}`,
  `POST /api/v1/admin/accounts/<id>/schedulable`, and
  `PUT /api/v1/admin/groups/<id>` with `model_routing` and
  `model_routing_enabled`. No request ever carries `extra`, `credentials`,
  `group_ids`, or `status`, because a full `extra` update in sub2api replaces
  the map and would wipe the passive usage data.
- Any fetch failure or missing account aborts the run before any write.
- All writes go through the audited admin API; nothing touches PostgreSQL or
  Redis directly.

## Limitations

- One priority per account: ordering uses the account-wide 7-day window.
  The Fable `7d_oi` window is used only for the reserve, not for ordering.
- Granularity is the timer interval; a five-minute cadence is plenty for
  session-level allocation but this is not a per-request scheduler.
- Passive usage data is only refreshed when an account serves a request.
  Stale usage is a safe lower bound; the reset timestamp is what matters.
- Verified against sub2api v0.2.0. Later versions may change the admin API
  or add native reset-aware scheduling, at which point this sidecar becomes
  unnecessary.

## Development

```sh
gofmt -l . && go vet ./... && go test ./...
```

Tests cover window normalization, urgent ranking and hysteresis, the reserve
and its release, routing ownership checks, the abort conditions, the exact
request bodies sent to the admin API, and shadow versus apply behavior.
