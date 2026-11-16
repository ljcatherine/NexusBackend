#!/usr/bin/env bash
# Nightly encrypted backup of the two things Nexus can't rebuild: the mailbox and the Keycloak database.
# Install: see README.md in this folder. Runs as root from nexus-backup.timer.
#   NEXUS_BACKUP_DIR         where backups go (an external disk or a mounted network share), required
#   NEXUS_BACKUP_PASSPHRASE  file holding the encryption passphrase (root-only), default /etc/nexus-backup/passphrase
#   NEXUS_BACKUP_KEEP        days to keep, default 14
set -euo pipefail
DEST=${NEXUS_BACKUP_DIR:?set NEXUS_BACKUP_DIR}
PASS=${NEXUS_BACKUP_PASSPHRASE:-/etc/nexus-backup/passphrase}
KEEP=${NEXUS_BACKUP_KEEP:-14}
[ -r "$PASS" ] || { echo "Passphrase file $PASS missing" >&2; exit 1; }
[ -d "$DEST" ] || { echo "Backup destination $DEST is not mounted" >&2; exit 1; }

stamp=$(date +%Y%m%d-%H%M%S)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"

# Mailbox: SQLite's online backup gives a consistent copy while the service runs.
if [ -f /var/lib/nexus-mail/mail.db ]; then
  python3 -c "import sqlite3,sys; s=sqlite3.connect(sys.argv[1]); d=sqlite3.connect(sys.argv[2]); s.backup(d); d.close(); s.close()" \
    /var/lib/nexus-mail/mail.db "$work/mail.db"
  [ -d /var/lib/nexus-mail/attachments ] && cp -a /var/lib/nexus-mail/attachments "$work/attachments"
fi
# Keycloak: users, credentials (hashed), realm settings.
if command -v pg_dump >/dev/null && id postgres >/dev/null 2>&1; then
  runuser -u postgres -- pg_dump --format=custom keycloak > "$work/keycloak.pgdump"
fi

out="$DEST/nexus-$stamp.tar.gz.gpg"
tar -C "$work" -czf - . | gpg --batch --yes --pinentry-mode loopback --passphrase-file "$PASS" \
  --symmetric --cipher-algo AES256 -o "$out"
chmod 600 "$out"
find "$DEST" -maxdepth 1 -name 'nexus-*.tar.gz.gpg' -mtime +"$KEEP" -delete
echo "Backup written: $out ($(du -h "$out" | cut -f1))"
