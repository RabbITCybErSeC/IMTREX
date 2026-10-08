# Building the Docker image

The published image **`autumn27/artex` is upstream's Chinese build** — it is produced by upstream's CI
and contains none of this translation. `docker compose up -d` out of the box therefore gives you the
Chinese interface. To run the English build under Docker you have to build the image yourself.

This is the recommended way to run this fork: unlike a native binary, the image carries the whole
recon toolchain (nmap, Playwright + chromium, dnsutils, whois, netcat, ripgrep, jq, Python 3.12,
Node 20) that the agents reach for through `Bash`.

---

## How the build is put together

The Dockerfile **compiles nothing**. It installs the tooling and copies in a Go binary you built
beforehand:

```dockerfile
COPY dist/${TARGETARCH}/artex /app/artex
```

So the real work happens before `docker build`, in three steps:

```
web/  --(next build, NEXT_EXPORT=1)-->  web/out
web/out  --(copy)-->  server/webui/dist
server/webui/dist  --(go build -tags embedui)-->  dist/<arch>/artex   <-- the image copies this
```

The frontend ends up **inside the binary** via `//go:embed all:webui/dist`. That is why
`.dockerignore` excludes `web/out` and `server/webui/dist`: the image never needs them, the binary
already carries them. It also means `server/webui/dist` has to exist at **Go build time**, not at
`docker build` time.

Upstream's CI does the same thing — it builds the Linux binaries in one job and the image job just
downloads them into `dist/amd64` and `dist/arm64` before building.

---

## Prerequisites

| | Version | Notes |
| --- | --- | --- |
| Go | >= 1.26.3 | see `go.mod` |
| Node | >= 20 | Playwright requires it; the image installs 20.x itself |
| Docker | with Compose v2 | `docker compose version` must work |
| Git Bash | Windows only | the npm script uses Unix env-var syntax |

---

## Build (amd64)

The common case: an x86-64 server or an Intel/AMD desktop.

```bash
# 1) static export of the frontend  (~3-4 min)
cd web && npm ci && npm run build:static && cd ..

# 2) copy it into the embed directory
rm -rf server/webui/dist && cp -r web/out server/webui/dist

# 3) cross-compile the LINUX binary the image expects  (~1 min, ~49 MB)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex

# 4) build the image  (~5-10 min the first time: apt + npm + chromium)
docker build -t artex-en:latest .
```

On **Windows** run exactly the same commands in **Git Bash**. In PowerShell, step 1 fails —
`npm run build:static` is `NEXT_EXPORT=1 next build`, which is Unix syntax. Use this instead and keep
the rest identical:

```powershell
cd web; npm ci; $env:NEXT_EXPORT="1"; npx next build; cd ..
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -tags embedui -o dist/amd64/artex ./cmd/artex
```

> `GOOS=linux` is not optional. The image copies a Linux ELF binary; building without it on Windows
> produces a PE executable and the container exits immediately with `exec format error`.

---

## Build (arm64, or both)

For an ARM server or Apple Silicon, swap the architecture — the directory name must match
`TARGETARCH`, so `arm64` goes in `dist/arm64`:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags embedui -o dist/arm64/artex ./cmd/artex
docker build -t artex-en:latest .
```

For a **multi-arch** image, build both binaries first, then use buildx. Only the apt/npm layer is
emulated, which is why upstream's CI does it this way:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags embedui -o dist/arm64/artex ./cmd/artex

docker buildx create --use --name artex-builder   # once
docker buildx build --platform linux/amd64,linux/arm64 -t artex-en:latest --load .
```

> `--load` with more than one platform needs the **containerd image store** (Docker Desktop:
> Settings -> General -> "Use containerd for pulling and storing images"; check with
> `docker info | grep snapshotter`). Without it the classic image store cannot hold a manifest list,
> so either `--push` to a registry or build one platform at a time.

---

## Run it

The compose file points at `autumn27/artex`, so override it. Copy the template rather than editing
the tracked file:

```bash
cp docker-compose.override.yml.example docker-compose.override.yml
cp .env.example .env            # set POSTGRES_PASSWORD
docker compose up -d
```

`docker compose` merges `docker-compose.override.yml` automatically. It contains:

```yaml
services:
  artex:
    image: artex-en:latest
    pull_policy: never
```

`pull_policy: never` matters: the image only exists locally, and without it compose tries to pull
`artex-en:latest` from Docker Hub and fails.

Then open **http://localhost:8787** (the first visit goes to `/setup` to set the admin password).

Verify you are on the right build:

```bash
docker compose exec artex /app/artex -h      # the help text is in English
```

---

## Upgrading

Rebuilding is the **only** upgrade path for this fork:

```bash
git pull
cd web && npm ci && npm run build:static && cd ..
rm -rf server/webui/dist && cp -r web/out server/webui/dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
docker build -t artex-en:latest .
docker compose up -d artex
```

Everything else would pull upstream's Chinese artifacts and silently replace your English build:

- **`docker compose pull`** — fetches `autumn27/artex`
- **`./update.sh`** — the Docker branch does `docker compose pull`
- **the one-click update in the UI** — downloads upstream's release binary from GitHub

The UI's "Version and updates" card also compares against upstream's tags, so it may report an update
that you do not want. Data is unaffected either way: the Postgres volume `pgdata`, `./data` and
`./skills` all survive a rebuild, and the schema re-applies itself idempotently on every start.

---

## What is in the image

Base `python:3.12-slim-bookworm`, plus:

- **Recon / network**: `nmap`, `dnsutils` (dig, nslookup), `whois`, `netcat-openbsd`, `telnet`, `ping`, `curl`, `wget`
- **Browser**: `@playwright/mcp`, `@playwright/cli`, `playwright` and **chromium with its system deps**, all preinstalled so nothing is downloaded at runtime
- **General**: `ripgrep`, `jq`, `git`, `unzip`, `vim`, Node 20, Python 3.12

Ports `8787` (UI + API) and `8788` (recording proxy). `./data` and `./skills` are bind-mounted from
the project root, so editing a skill needs no rebuild — the container reads the host copy.

To add a tool the agents should reach through `Bash`, extend the apt line in the `Dockerfile`; the
comment there already marks it as the place to adjust.

---

## Troubleshooting

**`COPY dist/amd64/artex: not found`**
Step 3 was skipped or wrote somewhere else. The path must be exactly `dist/<TARGETARCH>/artex` —
`amd64`, not `x86_64`.

**`exec /app/artex: exec format error`**
The binary is for the wrong platform. Rebuild with `GOOS=linux` and a `GOARCH` matching the image's
architecture.

**The UI shows "the frontend is not embedded in this binary"**
`-tags embedui` was missing from the `go build`.

**`pattern all:webui/dist: no matching files found`**
Step 2 was skipped — `server/webui/dist` has to exist before the Go build.

**The interface is still Chinese**
Compose is still using `autumn27/artex`. Check that `docker-compose.override.yml` exists (not just
the `.example`), then `docker compose up -d --force-recreate artex`.

**`Error response from daemon: pull access denied for artex-en`**
`pull_policy: never` is missing from the override, or you ran `docker compose pull`.
