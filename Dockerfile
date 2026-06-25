# syntax=docker/dockerfile:1
#
# Multi-stage build for the warden daemon. The final image is a lean Alpine
# runtime carrying just the static binary plus its runtime dependencies
# (tmux + git). Build from the repo root:
#
#   docker build -t warden:latest .
#
# See the "Run the daemon in Docker" section of README.md for usage.

# ---- Stage 1: build the web dashboard (embedded into the binary) ----
FROM node:22-alpine AS web
WORKDIR /src/web
# Install deps first so this layer caches across source-only changes.
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# ---- Stage 2: build the static warden binary ----
FROM golang:1.26-alpine AS build
WORKDIR /src
# Cache module downloads independently of the source tree.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Bring in the dashboard built above so go:embed (web/embed.go -> all:dist)
# bakes the real GUI into the binary instead of the .gitkeep placeholder.
COPY --from=web /src/web/dist ./web/dist
# CGO-free static build → runs on a minimal runtime image with no libc surprises.
RUN CGO_ENABLED=0 go build -buildvcs=false -ldflags "-s -w" -o /out/warden ./cmd/warden

# ---- Stage 3: lean runtime ----
FROM alpine:3.20
# tmux: warden drives every agent inside a tmux session — it is a hard runtime
#       dependency even for a daemon that only manages/monitors sessions.
# git:  worktree-isolated agents are created with `git worktree`.
# ca-certificates: outbound TLS to the LLM API and GitHub.
RUN apk add --no-cache tmux git ca-certificates
# Run as an unprivileged user; HOME drives the ~/.warden state directory.
ENV HOME=/home/warden
RUN adduser -D -h /home/warden warden \
    && mkdir -p /home/warden/.warden \
    && chown -R warden:warden /home/warden
USER warden
WORKDIR /home/warden
COPY --from=build /out/warden /usr/local/bin/warden
# Persist the session store and config across container restarts.
VOLUME ["/home/warden/.warden"]
EXPOSE 8765
# Bind all interfaces so the dashboard/API is reachable from outside the
# container. A non-loopback bind REQUIRES a bearer token: set WARDEN_TOKEN in
# the environment (see docker-compose.yml) or the daemon refuses to start.
ENTRYPOINT ["warden", "daemon", "--addr", "0.0.0.0:8765"]
