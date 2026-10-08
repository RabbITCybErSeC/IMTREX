#!/usr/bin/env bash
# ARTEX update script: (1) Docker update (pull a new image and recreate)  (2) local rebuild (rebuild the binary)
# The counterpart to install.sh: install handles the first deployment, update handles upgrading to a new version.
# DB migrations need no manual step -- artex idempotently re-runs schema.sql on every start (using
# ADD COLUMN / CREATE INDEX IF NOT EXISTS), so "restart is migrate". Data (the pgdata volume, ./data, ./skills) is untouched.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# -- Optional: sync the repository to the latest code (compose / scripts / local build sources all rely on this) -----
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull the latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or a diverged branch) -- resolve it manually and retry; continuing with the current code"
  fi
}

# -- (1) Docker update ----------------------------------------------
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not found; run ./install.sh to deploy first"
  [ -f .env ] || die ".env not found; run ./install.sh to complete the first deployment"

  # Optional: upgrade to a specific version tag (leave empty to keep ARTEX_TAG from .env, defaulting to latest)
  local tag; tag="$(ask 'Target image tag (Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG set to ${tag}"
  fi

  # Only touch artex: postgres is pinned to 16-alpine and does not need upgrading (pulling it is a
  # waste of bandwidth, and a major version change carries compatibility risk). artex declares
  # depends_on postgres, so bringing it up by service name starts pg if it is down and leaves a
  # running one as is, without recreating it.
  info "Pulling the new image (artex only)..."
  docker compose pull artex
  info "Recreating and starting (artex migrates the schema automatically on restart)..."
  docker compose up -d artex
  ok "Update complete -> http://localhost:8787"
  info "View logs: docker compose logs -f artex"
  info "Clean up old images (optional): docker image prune -f"
}

# -- (2) local rebuild ----------------------------------------------
update_local(){
  command -v go >/dev/null 2>&1 || die "Go (>=1.26) not found: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found -- for a first deployment use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding the static frontend assets..."
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Rebuilding the single binary with the frontend embedded..."
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: building a backend **without the frontend embedded** (run npm run dev separately for the frontend)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete -> ./artex"
  warn "Restart the running artex process for it to take effect (the schema migrates automatically on restart)"
}

echo "=============================="
echo "  ARTEX update"
echo "  1) Docker update (pull a new image and recreate)"
echo "  2) Local update (rebuild with go)"
echo "=============================="
case "$(ask 'Choice' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid choice" ;;
esac
