# XB 7-Day Quota Scheduler Design

Date: 2026-09-04. Status: approved and implemented. Originally written in the sub2cc repository; the implementation now lives in this repository.

## Goal

Keep the operator's normal Claude account precedence inside the `xb` group
(sub2api group 12), but automatically promote a direct subscription whose
unused 7-day capacity is about to expire, and enforce a private reserve on the
personal subscription. Existing sticky sessions must not be moved by ordering
changes; only the personal-subscription hard cut may move sessions.

## Verified constraints (sub2api v0.2.0)

- New-session selection compares the global `accounts.priority` (lower first),
  then load, then LRU. `account_groups.priority` is never compared.
- Sticky bindings (1h TTL, refreshed per use) are checked before priority.
  Only `schedulable=false`, temp-unschedulable, or a model rate limit breaks a
  binding. Group model routing outranks sticky.
- The native scheduling threshold is one percentage per platform shared by the
  5h, 7d, and 7d_oi windows, evaluated at selection time from cached `extra`.
  It cannot express different ceilings per window for one account.
- Admin API: `x-api-key` admin key; `GET /api/v1/admin/accounts` returns
  `extra` (passive 7d fields), `priority`, `schedulable`, `account_groups`;
  `PUT /api/v1/admin/accounts/:id` partial update (`priority` only; never send
  `extra`); `POST /api/v1/admin/accounts/:id/schedulable`; group routing via
  `PUT /api/v1/admin/groups/:id` (`model_routing`, `model_routing_enabled`),
  prefix wildcard patterns such as `claude-fable-*`.
- Passive extras: utilization is a 0-1 fraction, reset is unix seconds,
  `passive_usage_sampled_at` is RFC3339. A new 5h window clears the 7d keys
  until the next sample.
- The VPS has no Node or Go runtime, Python 3.13, 1 CPU, about 240 MB free
  memory, and already runs systemd timers. sub2api listens on loopback 8080.

## Component

A single static Go binary, `sub2cc-quota-scheduler`, kept in this repository
under `quota-scheduler/` with its own `go.mod` (standard library only). It is
cross-compiled from macOS for `linux/amd64`, installed under
`/opt/sub2cc-scheduler`, and run by a systemd timer every five minutes as a
dynamic unprivileged user. It talks only to `http://127.0.0.1:8080`.

The Node.js runtime rule in `CLAUDE.md` applies to the router and CLI; the
scheduler is a separately deployed component.

### Commands

- `sub2cc-quota-scheduler run --config <path>`: one evaluation, writes only
  in `apply` mode.
- `sub2cc-quota-scheduler plan --config <path>`: same evaluation, never
  writes, prints the decision as JSON.
- `sub2cc-quota-scheduler version`.

### Configuration (`/etc/sub2cc-scheduler/config.json`)

```json
{
  "base_url": "http://127.0.0.1:8080",
  "admin_key_env": "SUB2CC_SCHEDULER_ADMIN_KEY",
  "mode": "shadow",
  "group_id": 12,
  "lookahead_hours": 72,
  "min_urgent_headroom_percent": 5,
  "hysteresis_ratio": 0.2,
  "default_ceiling_percent": 95,
  "default_fable_ceiling_percent": 95,
  "fable_model_pattern": "claude-fable-*",
  "accounts": [
    { "id": 9,  "name": "relay",     "kind": "relay" },
    { "id": 11, "name": "ying",      "kind": "subscription" },
    { "id": 4,  "name": "xb-claude", "kind": "subscription" },
    { "id": 8,  "name": "my-team",   "kind": "subscription" },
    { "id": 1,  "name": "my-sub",    "kind": "subscription",
      "ceiling_percent": 60, "fable_ceiling_percent": 80, "enforce_ceiling": true }
  ]
}
```

`accounts` order is the base precedence. `mode` is `shadow` or `apply`. The
admin key is read from the named environment variable, supplied by a root-only
`EnvironmentFile`.

## Evaluation (every run)

1. Fetch all accounts and the group. Abort without writes if any policy
   account is missing, is not bound to `group_id`, or the group fetch fails.
2. Normalize windows per subscription account, for 7d and 7d_oi separately:
   - Missing utilization or reset: `unknown`.
   - Reset in the past: `rolled`; used = 0, reset advanced by whole weeks
     until it is in the future.
   - Otherwise `known` with used percent = fraction * 100.
   Sample age is logged only.
3. Urgent tier: subscription accounts with a `known` or `rolled` 7d window,
   `hours_to_reset <= lookahead_hours`, and
   `headroom = ceiling_percent - used >= min_urgent_headroom_percent`.
   `pressure = headroom / max(hours_to_reset, 1)`. Sort by pressure
   descending, then earlier reset, then base index. Hysteresis: two urgent
   accounts keep their previous relative order unless the higher pressure
   exceeds the lower by more than `hysteresis_ratio`.
4. Target order = urgent tier, then every other policy account in base order.
   Priority values are 1..N by position.
5. Personal reserve (accounts with `enforce_ceiling`):
   - 7d `known` and used >= `ceiling_percent`: ensure `schedulable=false`;
     remember `disabled_until = reset` in state.
   - State says disabled and now >= `disabled_until` or the window `rolled`:
     restore `schedulable=true` only if the account is still disabled; clear
     state. Never enable an account the scheduler did not disable.
   - 7d_oi `known` and used >= `fable_ceiling_percent`: set group routing
     `{fable_model_pattern: [other policy account IDs]}` with
     `model_routing_enabled=true`; remember `fable_until = reset`.
   - State says routing active and now >= `fable_until` or the window
     `rolled`: restore empty routing and `model_routing_enabled=false`.
   - If the live routing differs from both empty and the scheduler's own
     value, do not touch routing and log a warning.
6. Writes (apply mode only): `PUT priority` for accounts whose live priority
   differs from the target; the schedulable toggle; the group routing update.
   Each request carries only the fields named above.
7. Emit one JSON decision line to stdout and append it to
   `$STATE_DIRECTORY/decisions.jsonl`; persist `$STATE_DIRECTORY/state.json`
   (`last_order`, `disabled_until`, `fable_until`, `last_run`).

## Out of scope

- Weighted splitting of new sessions among urgent accounts.
- Fable-specific ordering (7d_oi is used only for the ceiling).
- Any change to sub2api itself, its database, or its service.
- The platform threshold 95 and admin key generation are one-time operator
  actions outside this component.

## Testing

`go test ./...` covers window normalization, urgent detection, pressure
ordering and hysteresis, reserve enforcement and release, routing ownership
checks, shadow versus apply write sets, and the abort conditions. The admin
client is tested against an `httptest` server that records request bodies.
