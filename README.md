# F1 Runner

Push a monorepo, run every component where it belongs.

`f1` is a single-binary deploy tool for monorepos. Each app in the repo carries its own
`f1.yml` manifest saying how it runs (Docker Compose or plain scripts); one root `f1.yml`
maps components to servers. `f1 deploy` fans out over SSH, each server pulls the repo with
a deploy key, builds only what changed, health-checks it, and flips a `current` symlink —
so `f1 rollback` is instant.

```
your machine                          server(s)
────────────                          ─────────
f1 deploy --all   ──ssh──▶  /opt/f1/bin/f1 apply
                            ├─ git fetch (deploy key)
                            ├─ skip unchanged components
                            ├─ materialize apps/web @ sha → releases/<stamp>/
                            ├─ setup → build → start (compose or scripts)
                            ├─ health check
                            └─ flip current → new release   (fail = old release keeps running)
```

## Install

```sh
make build        # ./f1 for your machine
make dist         # dist/f1-linux-amd64, -arm64, … (server setup uploads these)
```

Requirements: your machine needs `git` and OpenSSH (`ssh`/`scp`) on PATH.
Servers need `git`, plus `docker` with the compose plugin for docker components.

## Quick start

```sh
cd your-monorepo
f1 init                        # scaffold the root f1.yml — edit servers + components
f1 init component apps/web    # scaffold a component manifest
git add -A && git commit && git push

f1 server setup                # per server: creates /opt/f1, uploads the f1 binary,
                               # generates a deploy key and prints it — add it to your
                               # repo host (GitHub → repo → Settings → Deploy keys)

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
```

## Root f1.yml

```yaml
project: myapp
repo: git@github.com:me/myapp.git   # what servers fetch; can be a server-local path
branch: main

servers:
  web1:    { host: 1.2.3.4, user: deploy }
  workers: { host: 5.6.7.8, user: deploy, port: 22, key: ~/.ssh/id_ed25519, root: /opt/f1 }

components:
  web:    { path: apps/web,    servers: [web1] }
  api:    { path: apps/api,    servers: [web1, workers] }   # same app on many servers
  worker: { path: apps/worker, servers: [workers] }
```

## Component f1.yml

Docker component:

```yaml
runtime: docker
docker: { compose: docker-compose.yml }
env_file: /opt/f1/env/web.env      # optional; lives on the server, never in the repo
health:
  url: http://localhost:8080/      # or cmd: "curl -fsS …"
  retries: 5
  interval: 3s
keep: 5                            # releases kept for rollback
```

Script component:

```yaml
runtime: script
scripts:
  setup: ./scripts/setup.sh        # optional
  build: npm ci && npm run build   # optional
  start: sh scripts/start.sh       # required — must RETURN (daemonize with nohup/systemd)
  stop:  sh scripts/stop.sh        # optional, runs on the old release before switching
  logs:  tail -n 100 "$F1_LOG"     # optional, used by `f1 logs`
health: { cmd: "kill -0 $(cat $F1_SHARED/worker.pid)" }
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
| …plus everything from `env_file`.                                     |

## How deploys behave

- **Only changed components deploy.** f1 diffs each component's path between the deployed
  sha and the new one; untouched components are skipped (`--force` overrides). Note: the
  diff covers the component directory only — if you change something outside it that the
  component depends on, use `--force`.
- **The ref is pinned once.** `f1 deploy` resolves the branch to a sha up front so every
  server ships the same commit even if someone pushes mid-deploy.
- **Failures don't take you down.** Docker: the new stack must pass health before the
  symlink flips; on failure the previous release is restored. Scripts: on a failed start
  or health check, f1 flips back and restarts the old release. Failed release dirs are
  kept for debugging.
- **Rollback is local and instant** — no fetch, no build: flip to the previous release
  and restart.
- **State lives on the server** (`/opt/f1/state.json`), so `f1 apply` also works from a
  server cron job or CI runner without your laptop.

## Server layout

```
/opt/f1/
  bin/f1            the runner itself (uploaded by `f1 server setup`)
  repo/             bare clone of the monorepo
  deploy_key(.pub)  read-only git deploy key
  env/              your env files (chmod 600 them)
  state.json        what is deployed
  apps/<comp>/releases/<stamp>/ | current -> … | shared/
```

If `/opt/f1` isn't writable by your deploy user, run once on the server:
`sudo mkdir -p /opt/f1 && sudo chown deploy /opt/f1`

## End-to-end test

With Docker running: `bash e2e/run.sh` — builds a privileged docker-in-docker "server"
with sshd, pushes the example monorepo to it, then exercises setup, deploy, skip
detection, selective redeploy, rollback, and logs. `--keep` leaves the server running.

## Not in v1 (planned)

Webhook/CI-triggered agent mode · `f1 env set` secret management · blue-green ports ·
Windows servers · dependency-ordered deploy graphs.

## License

[F1 Runner License 1.0](LICENSE.md) — a custom source-available license:

- **Personal and commercial use** — free, royalty-free.
- **Feature modifications may stay private** — build on it without publishing anything.
- **Security and safety changes must be shared** — contribute them to this repository
  or publish them in a public fork, so everyone deploying with f1 stays safe.

Not an OSI-approved open-source license (the security-disclosure rule). Read
[LICENSE.md](LICENSE.md) for the exact terms.
