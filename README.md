# F1 Runner

Push a monorepo, run every component where it belongs.

`f1` is a single-binary deploy tool for monorepos. Each app in the repo carries its own
`f1.yml` manifest saying how it runs (Docker Compose or plain scripts); one root `f1.yml`
maps components to servers. `f1 deploy` fans out over SSH, each server pulls the repo with
a deploy key, installs whatever runtimes the app needs, builds only what changed,
health-checks it, and flips a `current` symlink — so `f1 rollback` is instant.

```
your machine                          server(s)
────────────                          ─────────
f1 deploy --all   ──ssh──▶  /opt/f1/bin/f1 apply        (or: CI webhook ──▶ f1 agent)
                            ├─ git fetch (deploy key)
                            ├─ dependency waves: db → api → web
                            ├─ skip unchanged components
                            ├─ provision runtimes (python, php, node, mariadb…)
                            ├─ materialize apps/web @ sha → releases/<stamp>/
                            ├─ setup → build → start (compose or scripts)
                            ├─ health check  (+ blue/green slot switch)
                            └─ flip current → new release   (fail = old release keeps running)
```

## Get it

**Download a prebuilt binary from [`dist/`](dist/)** — no toolchain needed:

| Platform        | Binary                          |
|-----------------|---------------------------------|
| Linux x86-64    | `dist/f1-linux-amd64`           |
| Linux ARM64     | `dist/f1-linux-arm64`           |
| macOS (Apple)   | `dist/f1-darwin-arm64`          |
| Windows x86-64  | `dist/f1-windows-amd64.exe`     |

Put it on your PATH (`install -m755 f1-linux-amd64 /usr/local/bin/f1`). Or build from
source: `make build` (your machine) / `make dist` (everything).

Your machine needs `git` and OpenSSH (`ssh`/`scp`) on PATH. Servers need `git` — and f1
can install everything else itself (see **Provisioning**).

## Quick start

```sh
cd your-monorepo
f1 init                        # scaffold the root f1.yml — edit servers + components
f1 init component apps/web    # scaffold a component manifest
git add -A && git commit && git push

f1 server setup                # per server: creates /opt/f1, uploads the f1 binary,
                               # generates a deploy key and prints it — add it to your
                               # repo host (GitHub → repo → Settings → Deploy keys),
                               # and installs everything under provision:

f1 deploy --all                # first deploy
```

Day to day:

```sh
f1 deploy web api              # deploy specific components (only if they changed)
f1 deploy --all --force        # redeploy everything regardless
f1 deploy --all --ref v1.2.0   # deploy a tag / branch / exact sha
f1 deploy --all --dry-run      # show what would happen
f1 status                      # merged table across all servers
f1 logs api -f                 # follow logs (compose logs / $F1_LOG / scripts.logs)
f1 rollback web                # instant flip to the previous release
f1 env set api DB_PASS=s3cret  # server-side secrets, never in the repo
```

## Root f1.yml

```yaml
project: myapp
repo: git@github.com:me/myapp.git   # what servers fetch; can be a server-local path
branch: main

servers:
  web1:
    host: 1.2.3.4
    user: deploy
    provision: [git, docker]        # f1 server setup installs these
  workers:
    host: 5.6.7.8
    user: deploy                    # port, key, root, ssh_opts also available
  win1:
    host: 9.9.9.9
    user: administrator
    os: windows                     # experimental — see Windows servers

components:
  db:     { path: apps/db,     servers: [workers] }
  api:    { path: apps/api,    servers: [web1, workers], depends_on: [db] }
  web:    { path: apps/web,    servers: [web1],          depends_on: [api] }
```

## Component f1.yml

Docker component:

```yaml
runtime: docker
docker: { compose: docker-compose.yml }
provision: [docker]                # ensure docker exists before deploying
health:
  url: http://localhost:8080/      # or cmd: "curl -fsS …"
  retries: 5
  interval: 3s
keep: 5                            # releases kept for rollback
```

Script component (PHP app under Apache, Python service, Node worker — anything):

```yaml
runtime: script
provision: [php, apache, mariadb]  # f1 installs these on the server first
scripts:
  setup: ./scripts/setup.sh        # optional
  build: composer install --no-dev # optional
  start: sh scripts/start.sh       # required — must RETURN (daemonize with nohup/systemd)
  stop:  sh scripts/stop.sh        # optional, runs on the old release before switching
  logs:  tail -n 100 "$F1_LOG"     # optional, used by `f1 logs`
shell: sh                          # sh | bash | cmd | powershell
health: { cmd: "curl -fsS http://localhost/health" }
```

Every lifecycle command runs from the release directory with:

| Variable       | Meaning                                              |
|----------------|------------------------------------------------------|
| `F1_RELEASE`   | this release's directory                             |
| `F1_CURRENT`   | the `current` symlink path                           |
| `F1_SHARED`    | persistent dir surviving releases (uploads, sqlite…) |
| `F1_LOG`       | conventional log file (`shared/app.log`)             |
| `F1_REF`       | the deployed commit sha                              |
| `F1_COMPONENT` | component name                                       |
| `F1_PORT`/`F1_SLOT` | live port + slot under blue/green               |
| …plus everything from `f1 env set` / `env_file`.                     |

## Provisioning

List what a component (or server) needs and f1 installs it — idempotently, with apt,
apk, or dnf/yum, as root or via passwordless sudo:

```
python · node (node@22 pins a major via NodeSource) · php (fpm + common extensions)
apache · nginx · mariadb (alias: mysql) · postgres · redis · docker · git · curl · build
```

- `servers.<name>.provision: [...]` installs during `f1 server setup`.
- component `provision: [...]` is (re)checked on the server before every deploy of it.
- Services are enabled and started where systemd/openrc exists. Python virtualenvs are
  the component's job (e.g. `setup: python3 -m venv $F1_SHARED/venv`), f1 guarantees
  `python3 -m venv` works.

## Secrets — f1 env

```sh
f1 env set api DB_PASS=s3cret SMTP_KEY=abc   # upserts on every server hosting api
f1 env unset api SMTP_KEY
f1 env show api
```

Values live in `<root>/env/<comp>.env` on each server (mode 600), never in the repo,
and are injected into every lifecycle command at the next deploy. A manifest may point
`env_file:` somewhere else; then that file must exist.

## Dependency-ordered deploys

`depends_on` builds waves: everything a component depends on deploys — and passes its
health check — before the component starts, across all servers (`db → api,worker → web`).
Cycles are rejected at config load. Components outside the requested set are assumed
already deployed.

## Blue-green (zero-downtime) deploys

```yaml
runtime: docker
blue_green:
  ports: [8001, 8002]              # blue port, green port
  switch: ./scripts/switch.sh      # optional: move traffic (rewrite upstream + reload)
docker: { compose: docker-compose.yml }
health: { url: "http://localhost:${F1_PORT}/health" }
```

Compose publishes `"${F1_PORT}:80"`. Each deploy goes to the *inactive* slot's port,
must pass health there, then the switch hook runs (gets `F1_PORT`, `F1_OLD_PORT`,
`F1_SLOT`) and the old slot is stopped. A failed deploy never touches the live slot.
Point your reverse proxy at both ports (or switch it in the hook).

## Webhook / CI agent mode

Run f1 resident on a server and let GitHub (or any CI) trigger deploys:

```sh
f1 agent --root /opt/f1 --repo git@github.com:me/myapp.git \
         --branch main --listen :9123 --token LONG_RANDOM_SECRET
```

- `POST /deploy` — GitHub push webhook (secret = token, HMAC verified): deploys pushes
  to `--branch` at the pushed sha. Or plain JSON from CI:
  `curl -H "Authorization: Bearer $TOKEN" -d '{"components":["web"],"force":true}' host:9123/deploy?wait=1`
  (`wait=1` streams the deploy log back; without it the webhook is acknowledged
  immediately and the deploy runs in the background).
- `GET /status`, `GET /healthz`.
- Deploys are serialized; each runs as a child `f1 apply`, so a bad deploy can't kill
  the agent. Systemd unit:

```ini
[Unit]
Description=f1 deploy agent
After=network.target
[Service]
Environment=F1_AGENT_TOKEN=LONG_RANDOM_SECRET
ExecStart=/opt/f1/bin/f1 agent --root /opt/f1 --repo git@github.com:me/myapp.git
Restart=always
[Install]
WantedBy=multi-user.target
```

## Windows servers (experimental)

Set `os: windows` on the server. f1 uploads `f1.exe`, uses `C:/f1` as the root, NTFS
junctions instead of symlinks, and `cmd`/`powershell` for scripts (`shell: powershell`
in the manifest). Requires Windows OpenSSH server and git. Provisioning and docker
runtime are not wired for Windows yet — script components only.

## How deploys behave

- **Only changed components deploy.** f1 diffs each component's path between the
  deployed sha and the new one (`--force` overrides). The diff covers the component
  directory only — change shared code outside it and you'll want `--force`.
- **The ref is pinned once** per `f1 deploy`, so every server and wave ships the same
  commit even if someone pushes mid-deploy.
- **Failures don't take you down.** The symlink flips only after start + health
  succeed; on failure f1 restores the previous release (or, under blue/green, simply
  never switches). Failed release dirs are kept for debugging.
- **Rollback is local and instant** — no fetch, no build.
- **State lives on the server** (`/opt/f1/state.json`), so `f1 apply` also works from
  cron or the agent without your laptop.

## Server layout

```
/opt/f1/
  bin/f1            the runner itself (uploaded by `f1 server setup`)
  repo/             bare clone of the monorepo
  deploy_key(.pub)  read-only git deploy key
  env/              secrets from `f1 env set` (mode 600)
  state.json        what is deployed
  apps/<comp>/releases/<stamp>/ | current -> … | shared/
```

If `/opt/f1` isn't writable by your deploy user, run once on the server:
`sudo mkdir -p /opt/f1 && sudo chown deploy /opt/f1`

## End-to-end test

With Docker running: `bash e2e/run.sh` — builds a privileged docker-in-docker "server"
with sshd, pushes the example monorepo to it, then exercises setup, deploy, skip
detection, selective redeploy, rollback, and logs. `--keep` leaves the server running.

## License

[F1 Runner License 1.0](LICENSE.md) — a custom source-available license:

- **Personal and commercial use** — free, royalty-free.
- **Feature modifications may stay private** — build on it without publishing anything.
- **Security and safety changes must be shared** — contribute them to this repository
  or publish them in a public fork, so everyone deploying with f1 stays safe.

Not an OSI-approved open-source license (the security-disclosure rule). Read
[LICENSE.md](LICENSE.md) for the exact terms.
