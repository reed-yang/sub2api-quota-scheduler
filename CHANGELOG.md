# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- A highest-priority tier for active, schedulable subscriptions whose 7d
  window resets within five hours and still has usable quota. Earlier reset
  wins, then greater headroom, without pressure hysteresis or a minimum
  unused percentage. Decisions expose `final_window` and explain promotion.

### Changed

- Starting a drain now requires at least `min_urgent_headroom_percent` of
  headroom, so a nearly exhausted account cannot move every live sticky
  session for a few remaining percent. A drain the scheduler already owns is
  unaffected and still runs to the account's ceiling.

### Fixed

- Unreserved accounts now use 100% capacity for ranking and drain selection;
  the default 95% reserve applies only to `enforce_ceiling` accounts. Explicit
  personal 7d/Fable reserves remain effective in the final five hours.
- Expired deadlines cannot enter urgency or drain selection, and inactive
  accounts cannot become drain targets.

## [0.3.1] - 2026-09-06

### Fixed

- The probe client now drains the test stream to EOF after `test_complete`.
  Hanging up on the first completion event cancelled sub2api's request
  context before its post-test account recovery ran (logged upstream as
  `context canceled`), so the documented rate-limit cleanup never happened.

## [0.3.0] - 2026-09-06

### Added

- Optional **idle window restart** (`restart_idle_windows`, default off):
  Anthropic anchors the 7-day window to the first message, so an account
  whose reset passed without traffic has no new window and, sitting behind
  the relay in the base order, never gets one. The scheduler now sends one
  probe message through sub2api's account test endpoint (`probe_model`,
  default `claude-haiku-4-5-20251001`) to such accounts, at most once per
  `probe_cooldown_hours` (default 6) and three per run, and treats the window
  as `estimated` (probe time plus seven days, rounded up to the hour) until
  sub2api samples anything newer. Accounts can opt out with `probe_exempt`;
  turning the feature off also drops every estimate.
- `probe` actions with `model` and `result` in the decision log; `probes` in
  the state file.

### Changed

- A passive 7-day reset in the past is now `idle` (full quota, no deadline)
  instead of `rolled`; the whole-week extrapolation is gone, so an idle
  account can no longer be promoted for a deadline that does not exist.
  Idle windows still release scheduler-imposed reserves.
- `HoursToReset` is `+Inf` for windows without a deadline; the decision log
  emits `hours_to_reset: null` and the full ceiling as `headroom_percent`
  for them.
- Run timeout raised from 90 s to 180 s; each probe has its own 60 s timeout.
- The state file is saved before the decision log is appended.

### Fixed

- Passive utilization is always a 0-1 fraction; values above 1 (an account
  over its limit, e.g. `1.03`) were misread as percents.

## [0.2.0] - 2026-09-05

### Added

- Optional **drain mode** (`drain_hours`, default off): when a non-exempt
  subscription's 7-day window resets within that many hours, the scheduler
  installs a group routing rule (`drain_model_pattern`, default `claude-*`)
  pointing at that account alone. sub2api evaluates routing before sticky
  sessions, so existing sessions move onto the draining account as well; when
  it is rate-limited sub2api falls back to normal priority selection and the
  routing pulls traffic back once it recovers. Accounts with `enforce_ceiling`
  or `drain_exempt` are never drained. The rule is removed at the reset.
- `drain_account_id` in the decision log; `routing_owned`, `drain_account_id`,
  `drain_until` in the state file.

### Changed

- Reserve and drain routing are reconciled as one routing map owned by the
  scheduler. Routing the scheduler did not write is still left untouched.

## [0.1.1] - 2026-09-04

### Changed

- Renamed the project from `sub2cc-quota-scheduler` to
  `sub2api-quota-scheduler`. The binary, systemd units, module path, and
  default paths follow the new name:
  `/opt/sub2api-quota-scheduler`, `/etc/sub2api-quota-scheduler`,
  `/var/lib/sub2api-quota-scheduler`, and the environment variable
  `SUB2API_QUOTA_SCHEDULER_ADMIN_KEY`.
- Dry-run warnings now name the configured mode; the version string is set
  at build time.

### Migration from 0.1.0

Run `deploy/migrate-from-0.1.0.sh` as root from a staging directory that also
holds the new binary, both unit files, and `install.sh`. It stops the old
timer, backs up `/etc/sub2cc-scheduler`, the state under
`/var/lib/private/sub2cc-scheduler` (systemd `DynamicUser` keeps the real
directory there; `/var/lib/sub2cc-scheduler` is only a symlink), and the old
unit files to `/root/sub2cc-quota-scheduler-backup`; renames the env key and
`admin_key_env`; moves the state; installs 0.1.1; runs a `plan` against live
data; and only then removes the old units and `/opt/sub2cc-scheduler` and
enables the new timer.

## [0.1.0] - 2026-09-04

### Added

- Urgent-tier ordering by expiring 7-day capacity with hysteresis.
- Per-account reserve: hard cut on the account-wide 7-day window and a
  Fable-only cut through group model routing.
- Shadow and apply modes, `plan --now` previews, JSON decision log.
- systemd service and timer, install script, cross-compile script.
- Tested against sub2api v0.2.0.
