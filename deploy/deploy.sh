#!/usr/bin/env bash
# Gate, build, ship, back up, migrate, install, health-check.
# First-time server setup and rollback are in docs/deploy.md.
#
#   SCRY_HOST=root@your-server make deploy
#
# SCRY_HOST is required. SCRY_SSH_KEY optionally names an identity file;
# without it ssh and scp use your normal SSH config and agent.
#
# The server-side steps (backup, migrate, install, health-check) are in
# deploy/install.sh, which this installs as /usr/local/sbin/scry-install.
set -euo pipefail
cd "$(dirname "$0")/.."
# go.mod replaces voodoo with ../voodoo for now: keep a voodoo checkout beside this one.

HOST="${SCRY_HOST:?set SCRY_HOST=user@host}"
SSH_OPTS=()
if [[ -n "${SCRY_SSH_KEY:-}" ]]; then SSH_OPTS=(-i "$SCRY_SSH_KEY"); fi

echo ">> gate: clean tree"
if [[ -n "$(git status --porcelain)" ]]; then
	git status --short >&2
	echo "refusing to deploy a dirty tree; commit or stash first" >&2
	exit 1
fi
command -v govulncheck >/dev/null || { echo "govulncheck not found; run 'make tools'" >&2; exit 1; }

echo ">> gate: vet, test, govulncheck"
go vet ./...
go test ./...
./deploy/vulncheck.sh

rev="$(git rev-parse --short HEAD)"
echo ">> building static binary at $rev"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -ldflags="-s -w" -o bin/scry-linux-amd64 ./cmd/scry

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
cp bin/scry-linux-amd64 "$STAGE/scry"
cp deploy/backup.sh deploy/scry.service "$STAGE/"

echo ">> uploading installer and release to $HOST"
scp ${SSH_OPTS[@]+"${SSH_OPTS[@]}"} deploy/install.sh "$HOST:/tmp/scry-install.new"
ssh ${SSH_OPTS[@]+"${SSH_OPTS[@]}"} "$HOST" 'install -m 0755 /tmp/scry-install.new /usr/local/sbin/scry-install && rm /tmp/scry-install.new'
tar -czf - -C "$STAGE" scry backup.sh scry.service |
	ssh ${SSH_OPTS[@]+"${SSH_OPTS[@]}"} "$HOST" sudo /usr/local/sbin/scry-install

echo ">> deployed $rev"
