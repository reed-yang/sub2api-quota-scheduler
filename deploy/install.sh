#!/bin/sh
# install.sh: run as root from a staging directory that contains the binary
# (sub2api-quota-scheduler, or the release asset sub2api-quota-scheduler-linux-<arch>),
# the two unit files, and optionally config.json (config.example.json is used on
# a first install). Idempotent; safe to re-run for upgrades.
set -eu
STAGE="$(cd "$(dirname "$0")" && pwd)"
NAME=sub2api-quota-scheduler
ETC=/etc/$NAME
OPT=/opt/$NAME
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
BIN="$STAGE/$NAME"
if [ ! -f "$BIN" ]; then
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) ARCH= ;;
  esac
  if [ -n "$ARCH" ] && [ -f "$STAGE/$NAME-linux-$ARCH" ]; then BIN="$STAGE/$NAME-linux-$ARCH"; fi
fi
[ -f "$BIN" ] || { echo "missing binary in $STAGE" >&2; exit 1; }
for f in $NAME.service $NAME.timer; do
  [ -f "$STAGE/$f" ] || { echo "missing $STAGE/$f" >&2; exit 1; }
done
chmod 0755 "$BIN"
"$BIN" version >/dev/null || { echo "staged binary does not run on this host" >&2; exit 1; }

# Never let two schedulers write to the same group: retire a 0.1.0 timer and
# pause our own timer while the binary is replaced.
systemctl disable --now sub2cc-quota-scheduler.timer 2>/dev/null || true
systemctl stop $NAME.timer 2>/dev/null || true

install -d -m 0755 "$OPT"
[ -d "$ETC" ] || install -d -m 0755 "$ETC"
install -m 0755 "$BIN" "$OPT/$NAME.new"
mv -f "$OPT/$NAME.new" "$OPT/$NAME"
if [ ! -f "$ETC/config.json" ]; then
  if [ -f "$STAGE/config.json" ]; then SRC="$STAGE/config.json"
  elif [ -f "$STAGE/config.example.json" ]; then SRC="$STAGE/config.example.json"
  else echo "missing config.json (or config.example.json) in $STAGE" >&2; exit 1; fi
  install -m 0644 "$SRC" "$ETC/config.json"
  echo "installed $ETC/config.json from $(basename "$SRC"); edit it before switching to apply mode" >&2
fi
if [ ! -f "$ETC/env" ]; then
  (umask 077; echo "SUB2API_QUOTA_SCHEDULER_ADMIN_KEY=" > "$ETC/env")
  echo "wrote empty $ETC/env; fill in the admin key" >&2
fi
install -m 0644 "$STAGE/$NAME.service" "$STAGE/$NAME.timer" /etc/systemd/system/
systemctl daemon-reload
if [ "${SUB2API_QS_NO_ENABLE:-0}" = 1 ]; then
  echo "units installed; timer left disabled (SUB2API_QS_NO_ENABLE=1)"
else
  systemctl enable --now $NAME.timer
  systemctl list-timers $NAME.timer --no-pager
fi
