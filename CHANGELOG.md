# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

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
