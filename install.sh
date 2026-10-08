#!/usr/bin/env bash
# ARTEX install script: (1) everything in Docker  (2) build and run locally
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# -- docker detection / automatic installation ---------------------
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker and docker compose detected"; return
  fi
  warn "docker / docker compose not found"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Install Docker automatically? (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker installed (log out and back in for the group change to take effect without sudo)"
      else
        die "Please install docker yourself and try again"
      fi ;;
    Darwin) die "On macOS, install Docker Desktop: https://www.docker.com/products/docker-desktop/" ;;
    *)      die "Please install docker yourself and try again" ;;
  esac
}

# -- (1) everything in Docker ---------------------------------------
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres password (Enter for a random one)' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY (may be left empty and configured in the UI later)' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env generated (POSTGRES_PASSWORD is set)"
  else
    info "Reusing the existing .env"
  fi
  info "Pulling images and starting..."
  docker compose pull || true
  docker compose up -d
  ok "Started -> http://localhost:8787"
  info "View logs: docker compose logs -f artex"
}

# -- (2) build and run locally --------------------------------------
install_local(){
  echo "Database setup:"
  echo "  1) Connect to an existing PostgreSQL"
  echo "  2) Start a PostgreSQL with Docker (requires docker)"
  case "$(ask 'Choice' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres password (Enter for a random one)' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask 'Database host' 127.0.0.1)"
      DB_PORT="$(ask 'Port' 5432)"
      DB_USER="$(ask 'User' artex)"
      DB_PASS="$(ask 'Password' '')"
      DB_NAME="$(ask 'Database name' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # Generate config.json
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json generated"

  # Check the Go toolchain
  command -v go >/dev/null 2>&1 || die "Go not found; please install Go (>=1.26) first: https://go.dev/dl/"
  ok "Go: $(go version)"

  # Embedding the frontend requires node to produce the static assets
  if command -v npm >/dev/null 2>&1; then
    info "Building the static frontend assets..."
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Building the single binary with the frontend embedded..."
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: building a backend **without the frontend embedded** (run npm run dev separately for the frontend)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete -> ./artex"

  info "Starting... (Ctrl-C to quit)"
  ./artex
}

echo "=============================="
echo "  ARTEX install"
echo "  1) Install everything in Docker"
echo "  2) Run locally (build with go)"
echo "=============================="
case "$(ask 'Choice' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "Invalid choice" ;;
esac
