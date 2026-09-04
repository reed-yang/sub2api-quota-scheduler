# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [0.1.0] - 2026-09-04

### Added

- Urgent-tier ordering by expiring 7-day capacity with hysteresis.
- Per-account reserve: hard cut on the account-wide 7-day window and a
  Fable-only cut through group model routing.
- Shadow and apply modes, `plan --now` previews, JSON decision log.
- systemd service and timer, install script, cross-compile script.
- Verified in production against sub2api v0.2.0.
