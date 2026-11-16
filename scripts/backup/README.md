# Nexus backups

Nightly, encrypted, off-box copies of the two things that can't be rebuilt: the mailbox
(`/var/lib/nexus-mail`) and the Keycloak database (users, hashed credentials, realm settings).

```sh
sudo apt install gnupg
sudo install -m 750 scripts/backup/nexus-backup.sh /usr/local/sbin/nexus-backup.sh
sudo install -d -m 700 /etc/nexus-backup
openssl rand -base64 48 | sudo tee /etc/nexus-backup/passphrase >/dev/null
sudo chmod 600 /etc/nexus-backup/passphrase
echo 'NEXUS_BACKUP_DIR=/mnt/backup/nexus' | sudo tee /etc/nexus-backup/nexus-backup.env
sudo install -m 644 scripts/backup/nexus-backup.service scripts/backup/nexus-backup.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nexus-backup.timer
sudo systemctl start nexus-backup.service && journalctl -u nexus-backup -n 5 --no-pager
```

**Save a copy of `/etc/nexus-backup/passphrase` somewhere safe that isn't the server** (your password
manager). Without it the backups can't be opened.

Restore:

```sh
gpg --batch --pinentry-mode loopback --passphrase-file /etc/nexus-backup/passphrase -d nexus-YYYYMMDD-HHMMSS.tar.gz.gpg | tar -xzf - -C /tmp/restore
```

Then stop `nexus-mail`, copy `mail.db` and `attachments/` back into `/var/lib/nexus-mail` (owner
`nexus-mail`), and for Keycloak: `pg_restore --clean -d keycloak /tmp/restore/keycloak.pgdump` as `postgres`.
