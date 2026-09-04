# Contributing

Thanks for your interest. This project is small on purpose: one static binary,
standard library only, no runtime dependencies on the host. Contributions that
keep it that way are very welcome.

## Ground rules

- Go standard library only. If a change needs a third-party module, open an
  issue first so we can discuss whether it is worth it.
- Every behavior change comes with a test. The policy code is pure and easy to
  test; see `policy_test.go` for the style.
- Never widen the set of fields written to sub2api without a very good reason.
  The scheduler sends `priority`, `schedulable`, and group `model_routing`
  only. In particular, never send `extra` or `credentials`.
- Keep `shadow` as the default mode and keep `plan` side-effect free.
- Code comments, docs, and commit messages are in English. Commit messages
  follow Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`).

## Workflow

1. Fork and branch from `main`.
2. `gofmt -l . && go vet ./... && go test ./...` must be clean.
3. If you touched `build.sh` or `deploy/`, run `sh -n` on the scripts and, if
   you can, a real shadow run against a sub2api instance.
4. Open a pull request. Describe the behavior change, not just the diff, and
   mention the sub2api version you tested against.

## Reporting sub2api compatibility problems

sub2api moves quickly. If an admin API field or endpoint changed, please
include the sub2api version, the request the scheduler made (redact the key),
and the response body. `journalctl -u sub2cc-quota-scheduler` has everything
except the key.

## Ideas that fit the scope

- Per-model ordering (using the `7d_oi` window for Fable-class models).
- Weighted allocation between several urgent accounts, if sub2api ever
  exposes a lever for it.
- Support for other platforms sub2api exposes 5h/7d windows for.

## Ideas that do not fit

- Anything that requires patching sub2api itself.
- Talking to PostgreSQL or Redis directly.
