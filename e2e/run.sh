#!/usr/bin/env bash
# End-to-end test: deploy the example monorepo to a dockerized "server".
# Usage: bash e2e/run.sh [--keep]   (--keep leaves the container running)
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$PWD"
WORK="$ROOT/e2e/.work"
SSH_PORT=2222

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
die() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }

say "build binaries"
go build -o "$WORK/f1.exe" . 2>/dev/null || { mkdir -p "$WORK"; go build -o "$WORK/f1.exe" .; }
GOOS=linux GOARCH=amd64 go build -o "$ROOT/dist/f1-linux-amd64" .
F1="$WORK/f1.exe"

say "test ssh key"
rm -f "$WORK/test_key" "$WORK/test_key.pub"
ssh-keygen -t ed25519 -N "" -q -f "$WORK/test_key"
chmod 600 "$WORK/test_key"
cp "$WORK/test_key.pub" e2e/authorized_keys

say "fake server container"
docker build -q -t f1-e2e e2e
docker rm -f f1-e2e >/dev/null 2>&1 || true
docker run -d --privileged --name f1-e2e -p $SSH_PORT:22 -e DOCKER_TLS_CERTDIR= f1-e2e >/dev/null

SSH="ssh -i $WORK/test_key -p $SSH_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR root@localhost"
say "wait for sshd + dockerd"
for i in $(seq 1 30); do $SSH true 2>/dev/null && break; sleep 1; [ "$i" = 30 ] && die "sshd never came up"; done
for i in $(seq 1 60); do $SSH docker info >/dev/null 2>&1 && break; sleep 1; [ "$i" = 60 ] && die "dockerd never came up"; done

say "monorepo copy + git push to the server"
rm -rf "$WORK/mono"
cp -r examples/monorepo "$WORK/mono"
cat > "$WORK/mono/f1.yml" <<EOF
project: example
repo: /srv/repo.git
branch: main
servers:
  prod:
    host: localhost
    port: $SSH_PORT
    user: root
    key: $WORK/test_key
    ssh_opts: ["-o","StrictHostKeyChecking=no","-o","UserKnownHostsFile=/dev/null","-o","LogLevel=ERROR"]
components:
  web:    { path: apps/web,    servers: [prod] }
  worker: { path: apps/worker, servers: [prod] }
EOF
$SSH git init --bare -b main /srv/repo.git >/dev/null
cd "$WORK/mono"
git init -q -b main
git config user.email e2e@test && git config user.name e2e
git add -A && git commit -qm "v1"
export GIT_SSH_COMMAND="ssh -i $WORK/test_key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
git push -q "ssh://root@localhost:$SSH_PORT/srv/repo.git" main

say "f1 server setup"
"$F1" server setup prod --binary "$ROOT/dist/f1-linux-amd64"

say "f1 deploy --all (v1)"
"$F1" deploy --all

say "verify v1"
$SSH curl -fsS http://localhost:8080/ | grep -q "example web v1" || die "web v1 not serving"
$SSH test -L /opt/f1/apps/web/current || die "web current symlink missing"
$SSH "kill -0 \$(cat /opt/f1/apps/worker/shared/worker.pid)" || die "worker not running"
"$F1" status | grep -q "web" || die "status missing web"
echo "v1 OK"

say "idempotent redeploy (should skip both)"
"$F1" deploy --all | grep -qi "skipping" || die "expected skip on unchanged redeploy"
echo "skip OK"

say "change web only -> deploy (worker must skip)"
sed -i 's/example web v1/example web v2/' apps/web/public/index.html
git commit -qam "v2 web"
git push -q "ssh://root@localhost:$SSH_PORT/srv/repo.git" main
OUT="$("$F1" deploy --all)"
echo "$OUT" | grep -q "worker: no changes" || die "worker should have been skipped"
$SSH curl -fsS http://localhost:8080/ | grep -q "example web v2" || die "web v2 not serving"
echo "selective deploy OK"

say "rollback web -> v1 serves again"
"$F1" rollback web
$SSH curl -fsS http://localhost:8080/ | grep -q "example web v1" || die "rollback did not restore v1"
echo "rollback OK"

say "logs"
"$F1" logs worker -n 5 | grep -q "worker beat" || die "worker logs missing"
echo "logs OK"

say "final status"
"$F1" status

if [ "${1:-}" != "--keep" ]; then
  say "cleanup"
  docker rm -f f1-e2e >/dev/null
fi
say "ALL E2E CHECKS PASSED"
