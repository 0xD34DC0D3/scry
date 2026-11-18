# Deploying scry

Same shape as voodoo's deployment: one static binary under systemd behind
Caddy, state in a SQLite file. scry shares voodoo's droplet. Steps that are
identical are not repeated; see [voodoo's deploy doc](../../voodoo/docs/deploy.md)
(`voodoo/docs/deploy.md`) for the Caddy install, firewall, and the S3 bucket,
age key and IAM user.

```
Internet ──▶ Caddy (:443) ──▶ scry (127.0.0.1:8081, systemd) ──▶ /var/lib/scry/scry.db
                                 │  outbound HTTPS: feeds, voodoo, Inngest Cloud
Inngest Cloud ──▶ Caddy ──▶ scry /api/inngest   (signed requests)
```

Placeholders: `scry.example.com` (scry's host; `scry.0xd34dc0d3.dev` in
production), `auth.example.com` (voodoo's host), `<server-ip>`,
`root@your-server`, `BUCKET_NAME`.

## First-time server setup

Once, on the droplet that already runs voodoo (Caddy, ufw, `sqlite3`, `age`
and `aws` are already there; if not, follow voodoo's doc).

1. **DNS.** A (and AAAA) record `scry.example.com` to `<server-ip>`, not
   proxied by a CDN, so Caddy can get its certificate. Check
   `dig +short scry.example.com`.

2. **User and data directory:**
   ```sh
   adduser --system --group --no-create-home --home /var/lib/scry scry
   install -d -o scry -g scry -m 0700 /var/lib/scry
   ```

3. **Env file:**
   ```sh
   install -d /etc/scry
   # copy deploy/scry.env.example to /etc/scry/scry.env, set the real values, then:
   chown root:scry /etc/scry/scry.env && chmod 0640 /etc/scry/scry.env
   ```
   Fill in `INNGEST_SIGNING_KEY` and `INNGEST_EVENT_KEY` from step 6. Leave
   `SCRY_PROD` unset (it defaults to on) and never set `INNGEST_DEV`. See
   [Configuration](#configuration).

4. **systemd unit:**
   ```sh
   # copy deploy/scry.service to /etc/systemd/system/scry.service, then:
   systemctl daemon-reload && systemctl enable scry
   ```
   Later deploys keep the unit in sync.

5. **Caddy.** Add the `scry.example.com` block from `deploy/Caddyfile` to
   `/etc/caddy/Caddyfile` next to voodoo's (never replace the file), then
   back up, `caddy validate`, and `systemctl reload caddy` as the comments in
   that file say.

6. **Inngest Cloud.** In the Inngest dashboard, in a production environment:
   copy the signing key and event key into `/etc/scry/scry.env`, and after the
   first deploy sync the app with URL `https://scry.example.com/api/inngest`
   (Apps, Sync new app). Re-sync after any deploy that adds or changes
   functions.

7. **Backups.** Create `/etc/scry/backup.env` (root:root 0600) from
   `deploy/backup.env.example`. Use the same bucket as voodoo, with prefix
   `scry/`, and the same or a new age key. The IAM policy in
   `deploy/aws/backup-iam-policy.json` is scoped to `BUCKET_NAME/scry/*`
   (edit the bucket name); attach it to voodoo's backup IAM user as a second
   statement, or create a second user. Add the same lifecycle rule for the
   `scry/` prefix (`deploy/aws/backup-lifecycle.json`; S3 holds one lifecycle
   configuration per bucket, so merge its rules into voodoo's, do not replace
   them). Cron, in root's crontab, offset from voodoo's:
   ```
   30 4 * * * /usr/local/bin/scry-backup
   ```
   Local-only mode (no off-box variables) works as for voodoo.

8. **First deploy**, from your machine with voodoo checked out beside scry
   (`go.mod` replaces voodoo with `../voodoo`):
   ```sh
   make tools
   SCRY_HOST=root@your-server make deploy
   ```

9. **Check:** `curl -i https://scry.example.com/healthz` returns 200, and
   `https://scry.example.com/` sends you to `auth.example.com` and back
   signed in. Voodoo must allow scry's host in its cookie domain
   (`VOODOO_COOKIE_DOMAIN=.example.com` covers it).

10. **Test a restore** once, as below.

## Deploying new code

```sh
SCRY_HOST=root@your-server make deploy
```

`SCRY_SSH_KEY` picks an identity file. Locally it refuses a dirty tree, runs
`go vet`, `go test` and `deploy/vulncheck.sh`, cross-compiles
`linux/amd64`, and uploads `deploy/install.sh` (as
`/usr/local/sbin/scry-install`) plus the release tarball (`scry`,
`backup.sh`, `scry.service`).

On the server, `scry-install`, in order:

1. installs the backup script and runs it; a failed backup stops the deploy
   with nothing changed;
2. installs the unit and reloads systemd, only if it changed;
3. stops scry (down until step 6);
4. runs `migrate up` with the new binary as user `scry`; on failure restarts
   the old binary and stops;
5. runs `scry doctor` with the new binary and the production env (config,
   voodoo's `/healthz` reachable, migrations applied); on failure restarts the
   old binary and stops. The migration from step 4 stays applied;
6. keeps the old binary as `/usr/local/bin/scry.prev`, installs the new one,
   starts scry, and polls `http://127.0.0.1:8081/healthz` (from `SCRY_BIND`
   and `SCRY_PORT`) for about ten seconds. On failure it prints the rollback
   command.

The Caddyfile is never deployed; edit it by hand (step 5 above).

### Rolling back

```sh
install -m 0755 /usr/local/bin/scry.prev /usr/local/bin/scry
systemctl restart scry
curl -fsS http://127.0.0.1:8081/healthz
```

Migrations are not reversed. If the old binary cannot run on the new schema,
restore the backup taken at the start of the deploy (newest file in
`/var/backups/scry`), skipping the `migrate up` step below.

### Deploying from CI

The `deploy` job in `.github/workflows/ci.yml` mirrors voodoo's. Because
`go.mod` replaces voodoo with `../voodoo`, CI checks out voodoo's `main` beside
scry and builds against it (see Known issues in [design.md](design.md)).
CI never uploads `install.sh`; changes to it reach the server only through
`make deploy`. One-time setup, after one `make deploy` so `scry-install` exists:

```sh
ssh-keygen -t ed25519 -N "" -C scry-ci -f ci_key
SCRY_HOST=root@your-server deploy/ci-setup.sh ci_key.pub
gh secret set DEPLOY_SSH_KEY --env production < ci_key
gh variable set DEPLOY_HOST --body deploy-scry@your-server
gh variable set DEPLOY_KNOWN_HOSTS --env production --body "$(ssh-keyscan -t ed25519 your-server)"
rm ci_key ci_key.pub
```

`DEPLOY_HOST` is a repository variable, not an environment one: the deploy
job's `if` reads it to stay dormant until it is set, and a job-level `if`
cannot see environment variables.

CI connects as `deploy-scry`, not voodoo's `deploy`, so the two keys do not
overwrite each other. Revoke by deleting `/home/deploy-scry/.ssh/authorized_keys`.

## Configuration

Every variable is in `deploy/scry.env.example` (production) and
`.env.example` (local):

- `SCRY_BASE_URL`, `SCRY_VOODOO_URL`: https in production; scry refuses to
  start otherwise.
- `SCRY_PORT` (8081) must match the Caddy `reverse_proxy`. `SCRY_BIND`
  defaults to `127.0.0.1`.
- `INNGEST_SIGNING_KEY`, `INNGEST_EVENT_KEY`: required in production.
  These are the Inngest SDK's own names.
- `SCRY_PROD`: leave unset.

## Operating

- Logs: `journalctl -u scry -f`; restart: `systemctl restart scry`
- Health: `curl https://scry.example.com/healthz`
- Config check on the server:
  `set -a; . /etc/scry/scry.env; set +a; sudo -u scry --preserve-env /usr/local/bin/scry doctor`

## Backups

`deploy/backup.sh` (installed as `/usr/local/bin/scry-backup`) is voodoo's
script with scry's names: `sqlite3 .backup` online copy, `integrity_check`,
local gzip in `/var/backups/scry` for 14 days, then age-encrypted upload to
`s3://BUCKET_NAME/scry/`, and an optional healthcheck ping. All
`SCRY_BACKUP_*` variables mirror voodoo's `VOODOO_BACKUP_*`; see the comments
in `deploy/backup.env.example` and voodoo's doc for the all-or-nothing rule and
local-only mode.

## Restoring from backup

Fetch and decrypt as in voodoo's doc (`aws s3 cp s3://BUCKET_NAME/scry/scry-STAMP.db.gz.age .`,
`age -d -i KEY ... | gunzip > scry.db`), check
`sqlite3 scry.db 'PRAGMA integrity_check;'` prints `ok`, then on the server:

```sh
systemctl stop scry
mv /var/lib/scry/scry.db /var/lib/scry/scry.db.broken
rm -f /var/lib/scry/scry.db-wal /var/lib/scry/scry.db-shm
install -m 0600 -o scry -g scry scry.db /var/lib/scry/scry.db
sudo -u scry env SCRY_DB_PATH=/var/lib/scry/scry.db /usr/local/bin/scry migrate up
systemctl start scry
```

Anything written after the backup is lost; scry can rebuild feed content by
re-fetching, but read state and subscriptions since then are gone.
