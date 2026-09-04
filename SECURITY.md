# Security

## Threat model

The scheduler holds a sub2api **admin API key**. Whoever has that key can
change any account or setting in your sub2api instance. The deployment in
`deploy/` is designed around that:

- The key lives in `/etc/sub2api-quota-scheduler/env`, `root:root`, mode `0600`,
  and is handed to the process by systemd through `EnvironmentFile`.
- The service runs as a dynamic unprivileged user with `ProtectSystem=strict`,
  `ProtectHome=yes`, `NoNewPrivileges=yes`, and may only connect to
  `127.0.0.1`.
- The key is never written to the decision log, the state file, or stdout.
- The scheduler only ever writes `priority`, `schedulable`, and group
  `model_routing`; it cannot create accounts, keys, or users.

Keep `base_url` on loopback or a private network. Do not expose the sub2api
admin API to the internet for this tool's sake.

## Reporting a vulnerability

Please open a GitHub issue with the label `security` for anything that is not
sensitive, or email the maintainer address listed on the GitHub profile for
anything that is. Expect an acknowledgement within a few days.
