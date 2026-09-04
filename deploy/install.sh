#!/bin/sh
# install.sh: run as root on the VPS from a staging
# directory containing the binary, units, and config. Idempotent.
set -eu
STAGE="$(cd "$(dirname "$0")" && pwd)"
install -d -m 0755 /opt/sub2cc-scheduler /etc/sub2cc-scheduler
install -m 0755 "$STAGE/sub2cc-quota-scheduler" /opt/sub2cc-scheduler/sub2cc-quota-scheduler
if [ ! -f /etc/sub2cc-scheduler/config.json ]; then
  install -m 0644 "$STAGE/config.json" /etc/sub2cc-scheduler/config.json
fi
if [ ! -f /etc/sub2cc-scheduler/env ]; then
  echo "SUB2CC_SCHEDULER_ADMIN_KEY=" > /etc/sub2cc-scheduler/env
  chmod 0600 /etc/sub2cc-scheduler/env
  echo "wrote empty /etc/sub2cc-scheduler/env; fill in the admin key" >&2
fi
install -m 0644 "$STAGE/sub2cc-quota-scheduler.service" /etc/systemd/system/
install -m 0644 "$STAGE/sub2cc-quota-scheduler.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now sub2cc-quota-scheduler.timer
systemctl list-timers sub2cc-quota-scheduler.timer --no-pager
