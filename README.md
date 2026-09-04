# sub2cc-quota-scheduler

A small static Go binary (standard library only) that keeps a group of Claude
subscription accounts in [sub2api](https://github.com/Wei-Shaw/sub2api)
ordered by *expiring 7-day capacity*, and enforces a private reserve on one
account, using only the sub2api admin API. It runs from a systemd timer next
to sub2api and needs no change to sub2api itself.

Why: sub2api v0.2.0 selects new sessions by static account priority and never
looks at the Anthropic 7-day reset time, so unused weekly quota on an account
that is about to reset is simply lost. This scheduler rewrites the priorities
every five minutes so that soon-to-reset capacity is consumed first, while
existing sticky sessions stay where they are.

Design notes: [docs/design.md](docs/design.md).

## How it decides

- Base precedence is the `accounts` order in the config.
- A subscription whose 7-day reset is within `lookahead_hours` and whose
  headroom below its ceiling is at least `min_urgent_headroom_percent` joins
  the urgent tier ahead of every other account. Urgent accounts are ordered by
  `headroom / hours_to_reset`; two urgent accounts keep their previous order
  unless the higher pressure exceeds the lower by `hysteresis_ratio`.
- Priorities `1..N` are written only for accounts whose live priority differs.
  Ordering changes never touch sticky sessions.
- Accounts with `enforce_ceiling` are set unschedulable when their 7-day
  usage reaches `ceiling_percent` and re-enabled after the reset only if the
  scheduler disabled them. When their Fable 7-day usage reaches
  `fable_ceiling_percent`, group model routing sends `fable_model_pattern` to
  the other policy accounts until that window resets. Routing owned by someone
  else is left alone with a warning.
- The ceiling for every other account should be sub2api's own platform
  scheduling threshold (for example 95%), configured once in the admin UI.
- Windows whose reset is already in the past are treated as rolled over:
  usage 0, reset advanced by whole weeks. Missing data means "unknown" and
  falls back to the base order.

## Configuration

See [deploy/config.example.json](deploy/config.example.json). Keys:

| Key | Default | Meaning |
| --- | --- | --- |
| `base_url` | `http://127.0.0.1:8080` | sub2api address, loopback recommended |
| `admin_key_env` | `SUB2CC_SCHEDULER_ADMIN_KEY` | environment variable holding the admin API key |
| `mode` | `shadow` | `shadow` logs decisions only; `apply` writes them |
| `group_id` | required | sub2api group whose accounts are managed |
| `lookahead_hours` | `72` | how early an expiring window becomes urgent |
| `min_urgent_headroom_percent` | `5` | minimum unused percent worth chasing |
| `hysteresis_ratio` | `0.2` | pressure gap required to reorder urgent accounts |
| `default_ceiling_percent` | `95` | 7d ceiling used for headroom |
| `default_fable_ceiling_percent` | `95` | Fable 7d ceiling used for headroom |
| `fable_model_pattern` | `claude-fable-*` | routing pattern for the Fable reserve |
| `accounts[]` | required | `id`, `name`, `kind` (`relay` or `subscription`), optional `ceiling_percent`, `fable_ceiling_percent`, `enforce_ceiling` |

Only the listed accounts are ever read for policy or written.

## Commands

```sh
sub2cc-quota-scheduler plan --config /etc/sub2cc-scheduler/config.json --state-dir /tmp/qs
sub2cc-quota-scheduler run  --config /etc/sub2cc-scheduler/config.json
sub2cc-quota-scheduler version
```

`plan` never writes. `run` writes only when `mode` is `apply`. Both print one
JSON decision, append it to `$STATE_DIRECTORY/decisions.jsonl`, and persist
`$STATE_DIRECTORY/state.json`.

## Build and test

```sh
gofmt -l . && go vet ./... && go test ./...
sh build.sh   # dist/sub2cc-quota-scheduler, linux/amd64, static
```

## Deploy

1. In the sub2api admin UI, generate an admin API key and set the platform
   scheduling threshold you want as the backstop (for example Anthropic 95).
2. Stage `dist/sub2cc-quota-scheduler`, `deploy/config.example.json` renamed
   to `config.json`, both unit files, and `deploy/install.sh` in one directory
   on the host, then run `sudo sh install.sh`.
3. Put the key in `/etc/sub2cc-scheduler/env` (`root:root`, `0600`).
4. `systemctl start sub2cc-quota-scheduler.service`, then inspect
   `journalctl -u sub2cc-quota-scheduler -n 5` and
   `/var/lib/sub2cc-scheduler/decisions.jsonl`.
5. After a clean shadow period, set `"mode": "apply"` in the config.

Rollback: `systemctl disable --now sub2cc-quota-scheduler.timer`; priorities
and the schedulable flag can be edited in the admin UI at any time.

## Requirements

- Go 1.22+ to build; nothing on the target host.
- sub2api v0.2.0 admin API (`x-api-key` admin key, `GET /api/v1/admin/accounts`,
  `PUT /api/v1/admin/accounts/:id`, `POST /api/v1/admin/accounts/:id/schedulable`,
  `GET`/`PUT /api/v1/admin/groups/:id`).
