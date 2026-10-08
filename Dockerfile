# syntax=docker/dockerfile:1
#
# Runtime image (nothing is compiled in the image): installs the common tooling and drops in a
# **prebuilt single Linux binary**.
# The binary is cross-compiled by CI's binaries job (pure Go, no QEMU) and placed in the build
# context at dist/<TARGETARCH>/artex. That way a multi-arch build only has to emulate the apt layer
# on arm64 instead of the Next/Go build, which is far faster.
#
# To build the image by hand locally, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tooling: ripgrep / curl / vim, plus a set of recon staples (adjust as needed).
# Node is installed from NodeSource at 20.x: the apt nodejs in bookworm is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and the CLI globally so nothing is downloaded over the network at runtime.
# @playwright/mcp: the browser MCP runs straight from `npx @playwright/mcp` (already installed globally, no -y/@latest needed).
# @playwright/cli: provides playwright-cli; --help is run afterwards to verify it is executable.
# Then install playwright itself (for browser management) and use --with-deps to preinstall chromium
# and its system dependencies, so MCP/CLI work on first start without downloading a browser.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Prebuilt binary for the matching architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Supervisor start script: when the process exits, the exit code decides whether it is restarted;
# the one-click update in the UI relies on it to swap the binary. It is also responsible for
# forwarding SIGTERM to artex -- docker stop only signals PID 1, and without forwarding, artex
# never receives it, cannot shut down gracefully, and is hard-killed by SIGKILL after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# Persistence point for data/ (SQLite + jwt.key)
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
