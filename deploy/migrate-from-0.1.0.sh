#!/bin/sh
# migrate-from-0.1.0.sh: move a live sub2cc-quota-scheduler 0.1.0 install to
# sub2api-quota-scheduler. Run as root from a staging directory that also
# contains the new binary, both unit files, and install.sh. The 0.1.0 files
# are copied to a backup before anything is changed. Safe to re-run.
set -eu
STAGE="$(cd "$(dirname "$0")" && pwd)"
OLD=sub2cc-scheduler
OLDUNIT=sub2cc-quota-scheduler
NEW=sub2api-quota-scheduler
OLD_ETC=/etc/$OLD
NEW_ETC=/etc/$NEW
OLD_STATE=/var/lib/private/$OLD
NEW_STATE=/var/lib/private/$NEW
OLD_OPT=/opt/$OLD
BACKUP=/root/$OLDUNIT-backup

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
for f in $NEW $NEW.service $NEW.timer install.sh; do
  [ -f "$STAGE/$f" ] || { echo "missing $STAGE/$f" >&2; exit 1; }
done
chmod 0755 "$STAGE/$NEW"
"$STAGE/$NEW" version >/dev/null || { echo "staged binary does not run on this host" >&2; exit 1; }
if [ -d "$OLD_ETC" ] && [ -d "$NEW_ETC" ]; then
  echo "both $OLD_ETC and $NEW_ETC exist; merge by hand" >&2; exit 1
fi
if [ ! -d "$OLD_ETC" ] && [ ! -d "$NEW_ETC" ]; then
  echo "nothing to migrate: $OLD_ETC does not exist" >&2; exit 1
fi

# 1. Stop the old scheduler, then back up everything before mutating it.
systemctl disable --now $OLDUNIT.timer 2>/dev/null || true
systemctl stop $OLDUNIT.service 2>/dev/null || true
if [ -d "$OLD_ETC" ]; then
  mkdir -p "$BACKUP"
  cp -a "$OLD_ETC" "$BACKUP/etc"
  if [ -d "$OLD_STATE" ]; then cp -a "$OLD_STATE" "$BACKUP/state"; fi
  if [ -d /var/lib/$OLD ] && [ ! -L /var/lib/$OLD ]; then cp -a /var/lib/$OLD "$BACKUP/state"; fi
  for u in $OLDUNIT.service $OLDUNIT.timer; do
    if [ -f /etc/systemd/system/$u ]; then cp -a /etc/systemd/system/$u "$BACKUP/"; fi
  done
  echo "0.1.0 install backed up under $BACKUP"
fi

# 2. Move config and env, rename the key, verify both rewrites.
if [ -d "$OLD_ETC" ]; then mv "$OLD_ETC" "$NEW_ETC"; fi
sed -i 's/^SUB2CC_SCHEDULER_ADMIN_KEY=/SUB2API_QUOTA_SCHEDULER_ADMIN_KEY=/' "$NEW_ETC/env"
chmod 0600 "$NEW_ETC/env"; chown root:root "$NEW_ETC/env"
grep -q '^SUB2API_QUOTA_SCHEDULER_ADMIN_KEY=.' "$NEW_ETC/env" || { echo "env key was not migrated" >&2; exit 1; }
sed -i 's/"admin_key_env"[[:space:]]*:[[:space:]]*"SUB2CC_SCHEDULER_ADMIN_KEY"/"admin_key_env": "SUB2API_QUOTA_SCHEDULER_ADMIN_KEY"/' "$NEW_ETC/config.json"
grep -q '"admin_key_env"[[:space:]]*:[[:space:]]*"SUB2API_QUOTA_SCHEDULER_ADMIN_KEY"' "$NEW_ETC/config.json" || { echo "config.json admin_key_env was not migrated" >&2; exit 1; }
if [ -f "$NEW_ETC/config.json.shadow-backup" ]; then mv "$NEW_ETC/config.json.shadow-backup" "$BACKUP/"; fi

# 3. Move state. With DynamicUser the real directory lives under
#    /var/lib/private and /var/lib/<name> is only a symlink.
if [ -d "$OLD_STATE" ] && [ ! -d "$NEW_STATE" ]; then
  mv "$OLD_STATE" "$NEW_STATE"
  rm -f /var/lib/$OLD
  ln -sfn private/$NEW /var/lib/$NEW
elif [ -d /var/lib/$OLD ] && [ ! -L /var/lib/$OLD ] && [ ! -e /var/lib/$NEW ]; then
  mv /var/lib/$OLD /var/lib/$NEW
fi
if [ ! -s "$NEW_STATE/state.json" ] && [ ! -s /var/lib/$NEW/state.json ]; then
  echo "warning: no state.json carried over; reserve bookkeeping restarts empty" >&2
fi

# 4. Install the new binary and units, timer still disabled.
SUB2API_QS_NO_ENABLE=1 sh "$STAGE/install.sh"

# 5. Dry run against live data before going live. The old install is still
#    fully restorable from $BACKUP at this point.
set -a; . "$NEW_ETC/env"; set +a
TMP="$(mktemp -d)"
if [ -d "$NEW_STATE" ]; then cp -a "$NEW_STATE/." "$TMP/"; fi
"/opt/$NEW/$NEW" plan --config "$NEW_ETC/config.json" --state-dir "$TMP" > "$TMP/plan.json"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print("plan: order=%s actions=%d warnings=%s" % (d["target_order"], len(d["actions"]), d["warnings"]))' "$TMP/plan.json" 2>/dev/null || cut -c1-300 "$TMP/plan.json"
rm -rf "$TMP"
unset SUB2API_QUOTA_SCHEDULER_ADMIN_KEY

# 6. Retire the old install and go live.
if [ -d "$OLD_OPT" ]; then mv "$OLD_OPT" "$BACKUP/opt"; fi
for u in $OLDUNIT.service $OLDUNIT.timer; do
  if [ -f /etc/systemd/system/$u ]; then rm -f /etc/systemd/system/$u; fi
done
systemctl daemon-reload
systemctl enable --now $NEW.timer
systemctl start $NEW.service || { journalctl -u $NEW -n 20 --no-pager -o cat; exit 1; }
systemctl list-timers $NEW.timer --no-pager --all | sed -n 2p
echo "done; 0.1.0 files kept under $BACKUP"
