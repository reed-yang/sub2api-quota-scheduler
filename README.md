# sub2api-quota-scheduler

[![CI](https://github.com/reed-yang/sub2api-quota-scheduler/actions/workflows/ci.yml/badge.svg)](https://github.com/reed-yang/sub2api-quota-scheduler/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/reed-yang/sub2api-quota-scheduler)](https://github.com/reed-yang/sub2api-quota-scheduler/releases)
[![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![sub2api](https://img.shields.io/badge/tested%20with-sub2api%20v0.2.0-blue)](https://github.com/Wei-Shaw/sub2api)

**Spend expiring Claude 7-day quota first.** A sidecar for
[sub2api](https://github.com/Wei-Shaw/sub2api) that reorders the accounts of
a group by *reset countdown and unused capacity*, and keeps a private reserve
on one account, using nothing but the sub2api admin API.

[中文说明](README.zh-CN.md)

- Single static Go binary, standard library only, ~6 MB, ~3 MB RSS, ~30 ms per run.
- Runs from a systemd timer next to sub2api; nothing to install on the host.
- Shadow mode by default: it tells you what it *would* change until you flip it.
- Never moves an existing sticky session by reordering; only the optional
  hard reserve can.
- Zero changes to sub2api, its database, or its service. Disable the timer
  and everything is back to ordinary sub2api behavior.

## Status

Young project: running in apply mode on a five-account Claude group since
2026-09-04, so expect rough edges and please report them. Tested against
sub2api **v0.2.0**; the admin endpoints it relies on are listed under
[Compatibility](#compatibility). This is an independent community project
and is not affiliated with the sub2api maintainers.

## The problem

Claude subscriptions (Max, Team, and similar) meter usage in a **7-day
window** that resets at a fixed time. Unused capacity at reset is simply lost.
When you pool several subscriptions behind sub2api, the useful question is not
"which account is preferred" but "which account's unused capacity is about to
vanish".

sub2api v0.2.0 cannot answer that on its own:

| What sub2api does | Consequence |
| --- | --- |
| New sessions pick the lowest **static priority**, then load, then LRU | The 7-day reset time is never a selection key |
| `prefer_soonest_reset` compares the **5-hour** window only | Weekly capacity still expires unused |
| It records the Anthropic `7d` / `7d_oi` headers per account | The data exists but nothing schedules on it |
| Its threshold pauses an account above one percentage | One number for all windows and all accounts; no per-account reserve |
| Sticky sessions (1 h, refreshed on use) outrank priority | Good: running sessions must not hop and lose their prompt cache |

The upstream request for reset-aware scheduling,
[Wei-Shaw/sub2api#5583](https://github.com/Wei-Shaw/sub2api/issues/5583), is
open; related asks are
[#979](https://github.com/Wei-Shaw/sub2api/issues/979) and
[#5681](https://github.com/Wei-Shaw/sub2api/issues/5681). Forking sub2api
means rebuilding and re-patching on every upstream release, and sub2api ships
several releases a week. This project takes the sidecar route instead: use the
data sub2api already collects and the admin API it already exposes.

Two concrete needs drove the design:

1. **Do not waste expiring capacity.** An account that resets in a day with
   70% unused should receive new sessions before an account that resets next
   week, even if the operator's normal preference says otherwise.
2. **Keep a private reserve.** One subscription is also used for the owner's
   own chat. Gateway traffic must never push it past a chosen level, with a
   separate, higher level for models that have their own sub-window (the
   Anthropic `7d_oi` window used by Fable-class models).

## How it works

Every run (default: every five minutes):

1. **Fetch** the group's accounts and model routing through the admin API.
   Abort without writing if any configured account is missing or not in the
   group.
2. **Normalize** each subscription's 7-day windows from the passive usage
   fields sub2api stores in `extra`:
   - `known`: utilization plus a future reset timestamp.
   - `idle`: the recorded reset is in the past and nothing has been sampled
     since. Anthropic anchors the 7-day window to the first message, so no
     new window exists yet: the account has its full quota, no deadline, and
     stays in the base order. The scheduler never extrapolates a reset.
   - `estimated`: the scheduler's own probe started the window (see step 7)
     and no sample has arrived yet; reset is probe time plus seven days,
     rounded up to the hour.
   - `unknown`: fields missing (sub2api clears them at each new 5-hour window
     until the next response is sampled); the account falls back to the base
     order and never triggers a reserve action.
3. **Rank the urgent tier**: subscriptions whose reset is within
   `lookahead_hours` and whose headroom below their ceiling is at least
   `min_urgent_headroom_percent`, ordered by
   `pressure = headroom / hours_to_reset`. Two urgent accounts keep their
   previous relative order unless the higher pressure exceeds the lower by
   more than `hysteresis_ratio`, so the order does not flap.
4. **Write priorities**: urgent tier first, then everyone else in the
   configured base order, as priorities `1..N`. Only accounts whose live
   priority differs are written, with a body containing nothing but
   `priority`.
5. **Enforce reserves** for accounts marked `enforce_ceiling`:
   - 7-day usage at or above `ceiling_percent`: set the account unschedulable
     until its window resets, then re-enable it. The scheduler remembers that
     it disabled the account and never re-enables one an operator disabled by
     hand.
   - Fable (`7d_oi`) usage at or above `fable_ceiling_percent`: install a
     group routing rule that sends `fable_model_pattern` to the other
     configured accounts, and remove it after the reset. Routing the
     scheduler did not write is left alone with a warning.
6. **Drain mode** (optional, `drain_hours`): when a non-exempt subscription
   resets within that many hours, install a group routing rule
   (`drain_model_pattern`, default `claude-*`) that points at that account
   alone. sub2api checks routing before sticky sessions, so *existing*
   sessions move onto the draining account too; if it gets rate-limited,
   sub2api falls back to normal priority selection, and the routing pulls
   traffic back as soon as it recovers. Each switch costs one prompt-cache
   miss, and relays are bypassed while draining. Accounts with
   `enforce_ceiling` or `drain_exempt` are never drained.
7. **Restart idle windows** (optional, `restart_idle_windows`): an `idle`
   subscription that is active, schedulable, and not `probe_exempt` gets one
   probe through sub2api's account test endpoint: a single "hi" message with
   `probe_model` (default `claude-haiku-4-5-20251001`). That is enough for
   Anthropic to open the next 7-day window, so the account can become urgent
   again a week later instead of sitting idle behind the relay forever. The
   test endpoint does not sample usage headers, so the scheduler records the
   probe (with the ended window it saw) in its state file and treats the
   window as `estimated` (probe time plus seven days, rounded up to the hour)
   until sub2api samples anything newer: a live window or a later ended one
   both retire the estimate. Accounts with no 7-day sample at all are probed
   the same way. Eligibility: active status, schedulable or being re-enabled
   by this run's reserve release, not `probe_exempt`. Attempts are spaced by
   `probe_cooldown_hours`, failures are recorded and retried after the
   cooldown, and a run sends at most three probes, each with its own 60 s
   timeout. Probes run last and never change the current run's order.
8. **Log** one JSON decision line to stdout and `decisions.jsonl`, and persist
   a small state file.

Everything except the reserve and drain mode is soft: it only changes where
**new** sessions land. sub2api's sticky-session logic keeps existing sessions on their account,
and its own rate-limit and threshold handling still applies on top.

### Worked example

Five accounts, base order `relay > sub-a > sub-b > team-plan > personal-max`,
`personal-max` reserved at 60%:

| Account | 7d used | Resets in | Headroom | Pressure | Result |
| --- | ---: | ---: | ---: | ---: | --- |
| team-plan | 25% | 21 h | 70 | 3.36 | urgent, priority 1 |
| personal-max | 35% | 72 h | 25 | 0.35 | urgent, priority 2 |
| relay | n/a | n/a | | | base, priority 3 |
| sub-a | 24% | 159 h | | | base, priority 4 |
| sub-b | 33% | 123 h | | | base, priority 5 |

When `team-plan` resets, it drops out of the urgent tier and `personal-max`
moves to priority 1 until it reaches 60% or resets. You can preview any
future moment with `plan --now`.

## Quick start

Requirements: Go 1.22+ on your workstation (or a
[release binary](https://github.com/reed-yang/sub2api-quota-scheduler/releases)),
sub2api v0.2.0 or compatible on the host, nothing else.

1. **Get the binary.** Download `sub2api-quota-scheduler-linux-amd64` (or
   `-arm64`) from the releases page, or build it:

   ```sh
   sh build.sh          # dist/sub2api-quota-scheduler, static linux/amd64
   ```

2. **Create an admin API key** in the sub2api admin UI (Settings, Admin API
   key, Regenerate). Optionally set the platform scheduling threshold there as
   a backstop for accounts without an explicit reserve.

3. **Write the config.** Copy `deploy/config.example.json` to `config.json`,
   set `group_id`, list the account IDs in your preferred base order, and add
   per-account ceilings where you want a reserve. Leave `mode` as `shadow`.

4. **Install on the host.** Put the binary (the release asset name works as
   is), `config.json`, both unit files from `deploy/`, and `deploy/install.sh`
   in one directory, then:

   ```sh
   sudo sh install.sh
   sudo sh -c 'umask 077; echo "SUB2API_QUOTA_SCHEDULER_ADMIN_KEY=admin-..." > /etc/sub2api-quota-scheduler/env'
   sudo systemctl start sub2api-quota-scheduler.service
   journalctl -u sub2api-quota-scheduler -n 3 -o cat
   ```

   The installer copies the binary to `/opt/sub2api-quota-scheduler`, the config to
   `/etc/sub2api-quota-scheduler/config.json`, and enables the timer. The service
   runs as a dynamic unprivileged user, may only reach `127.0.0.1`, and keeps
   its state in `/var/lib/sub2api-quota-scheduler`.

5. **Watch shadow decisions** for a while:

   ```sh
   sudo tail -n 1 /var/lib/sub2api-quota-scheduler/decisions.jsonl | python3 -m json.tool
   ```

6. **Enable writes** by changing `"mode": "shadow"` to `"mode": "apply"` in
   `/etc/sub2api-quota-scheduler/config.json`. The next run applies the actions; a
   run with nothing to change performs no writes.

Rollback at any time: `sudo systemctl disable --now sub2api-quota-scheduler.timer`.
Priorities, the schedulable flag, and group routing remain ordinary fields
you can edit in the sub2api admin UI.

## Configuration

See [`deploy/config.example.json`](deploy/config.example.json).

| Key | Default | Meaning |
| --- | --- | --- |
| `base_url` | `http://127.0.0.1:8080` | sub2api address; keep it on loopback |
| `admin_key_env` | `SUB2API_QUOTA_SCHEDULER_ADMIN_KEY` | environment variable that holds the admin API key |
| `mode` | `shadow` | `shadow` logs only; `apply` writes |
| `group_id` | required | the sub2api group whose accounts are managed |
| `lookahead_hours` | `72` | how early an expiring window becomes urgent |
| `min_urgent_headroom_percent` | `5` | minimum unused percent worth promoting for |
| `hysteresis_ratio` | `0.2` | relative pressure gap needed to reorder two urgent accounts |
| `default_ceiling_percent` | `95` | 7d ceiling used for headroom when an account sets none |
| `default_fable_ceiling_percent` | `95` | `7d_oi` ceiling used when an account sets none |
| `fable_model_pattern` | `claude-fable-*` | routing pattern installed for the Fable reserve |
| `drain_hours` | `0` (off) | drain mode: route everything to a subscription that resets within this many hours |
| `drain_model_pattern` | `claude-*` | routing pattern installed while draining |
| `restart_idle_windows` | `false` | probe idle subscriptions so Anthropic starts their next 7-day window |
| `probe_model` | `claude-haiku-4-5-20251001` | model used for the one-message probe |
| `probe_cooldown_hours` | `6` | minimum gap between probe attempts on one account |
| `accounts[]` | required | ordered list; each has `id`, `name`, `kind` (`relay` or `subscription`), optional `ceiling_percent`, `fable_ceiling_percent`, `enforce_ceiling`, `drain_exempt`, `probe_exempt` |

`relay` accounts (API-key relays with no visible quota) always stay in the
base order. Only the listed accounts are ever read for policy or written.

## Commands

```sh
sub2api-quota-scheduler run  --config /etc/sub2api-quota-scheduler/config.json
sub2api-quota-scheduler plan --config /etc/sub2api-quota-scheduler/config.json --state-dir /tmp/qs --now 2026-09-05T05:30:00Z
sub2api-quota-scheduler version
```

`plan` never writes, whatever the config says, and accepts `--now` to preview
a future point in time against live data. `--state-dir` defaults to systemd's
`$STATE_DIRECTORY`.

## Safety properties

- Reads: `GET /api/v1/admin/accounts?group=<id>` and `GET /api/v1/admin/groups/<id>`.
- Writes, apply mode only: `PUT /api/v1/admin/accounts/<id>` with
  `{"priority": n}`, `POST /api/v1/admin/accounts/<id>/schedulable`, and
  `PUT /api/v1/admin/groups/<id>` with `model_routing` and
  `model_routing_enabled`. No request ever carries `extra`, `credentials`,
  `group_ids`, or `status`; a full `extra` update in sub2api replaces the map
  and would wipe the passive usage data.
- With `restart_idle_windows`: `POST /api/v1/admin/accounts/<id>/test` with
  `{"model_id": ...}`, at most once per cooldown per account and three per
  run, only for a window sub2api's last sample shows as ended (or an account
  with no sample). This sends one real message through the account (a few
  hundred tokens). Side effects inside sub2api: a successful test clears the
  account's rate-limit bookkeeping; an upstream 403 makes sub2api set the
  account's status to `error`, which removes it from scheduling everywhere,
  and the scheduler will neither probe nor re-enable it afterwards.
- Any fetch failure or missing account aborts the run before any write.
- All writes go through the audited admin API; nothing touches PostgreSQL or
  Redis directly.
- The admin key is read from the environment and never logged. See
  [SECURITY.md](SECURITY.md).

## Compatibility

| sub2api | Status |
| --- | --- |
| v0.2.0 | Tested; in daily use by the author |
| Older 0.1.x | Untested; the admin endpoints above existed for a while but check `extra` field names |
| Newer | Please report; if upstream ships native reset-aware scheduling this project can retire |

Platform: any Linux with systemd (`amd64` and `arm64` binaries are released).
The scheduler itself is a plain executable and can be run by cron or by hand.

## Limitations

- One priority per account: ordering uses the account-wide 7-day window. The
  Fable `7d_oi` window is used only for the reserve, not for ordering.
- Granularity is the timer interval. Five minutes is plenty for session-level
  allocation; this is not a per-request scheduler.
- Passive usage data is refreshed only when an account serves a request.
  Stale usage is a safe lower bound; the reset timestamp is what matters.
  sub2api's "active" usage query only reaches Anthropic for `oauth`
  accounts; for `setup-token` accounts it returns a local estimate, so the
  scheduler does not use it.
- After a probe the window is an estimate (probe time plus seven days,
  rounded up to the hour) until the first real response through that
  account. If Anthropic anchors the window differently for your plan, the
  sampled reset wins.
- In the decision log, `hours_to_reset` is `null` and `headroom_percent` is
  the full ceiling for windows without a deadline.
- It can only steer traffic that exists. If nobody sends requests, expiring
  capacity still expires.

## Roadmap

- [x] Drain mode: move existing sessions onto an expiring account (0.2.0).
- [x] Restart idle windows with a one-message probe (0.3.0).
- [ ] Per-model ordering using the `7d_oi` window for Fable-class models.
- [ ] Weighted allocation across several urgent accounts, if sub2api exposes
      a lever for it (see [#979](https://github.com/Wei-Shaw/sub2api/issues/979)).
- [ ] Other platforms for which sub2api records 5h/7d windows.
- [ ] Optional Prometheus-style metrics endpoint for the decision log.

Have a use case? Open an issue with the situation and the decision log line.

## FAQ

**Will it move my running Claude Code session to another account?**
Not by reordering: sub2api's sticky binding keeps existing sessions in place.
Two optional features can move sessions, and both are off unless you enable
them: the hard reserve on an account you marked `enforce_ceiling`, and drain
mode, which deliberately pulls every session onto an account whose weekly
window is about to reset.

**Why does an urgent account still not get all the traffic?**
Three things the scheduler cannot change: sub2api rewrites a session's sticky
binding when a request fails over (one transient upstream error is enough),
an account that hits its 5-hour limit is excluded until that window ends, and
a 7-day window simply cannot be drained in a few hours. Drain mode addresses
the first two by routing rather than ordering.

**Why does an account that just reset get no traffic for days?**
Because its next 7-day window has not started: Anthropic opens it at the
first message, and an account sitting behind the relay in the base order
never sends one. Enable `restart_idle_windows` and the scheduler sends that
first message itself, so the account becomes urgent again a week later.

**Why not patch sub2api?**
Because you would have to re-patch on every release. The sidecar uses public
admin endpoints and stable `extra` field names, and is a one-line disable if
anything drifts.

**Does it need database or Redis access?**
No. Loopback HTTP to the admin API is the only dependency.

**Can I run it from cron instead of systemd?**
Yes: run `sub2api-quota-scheduler run --config ... --state-dir <dir>` with the
key in the environment. The systemd unit just adds sandboxing.

## Contributing

Bug reports with a decision log line, compatibility notes for other sub2api
versions, and small focused pull requests are all welcome. See
[CONTRIBUTING.md](CONTRIBUTING.md). Development loop:

```sh
gofmt -l . && go vet ./... && go test ./...
```

## Related

- [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api), the gateway this
  project schedules for.
- [sub2api#5583](https://github.com/Wei-Shaw/sub2api/issues/5583), the
  upstream request for unified 5h/7d reset-aware scheduling.

## License

[MIT](LICENSE). This project does not include or link any sub2api code; it
talks to sub2api over its HTTP admin API.
