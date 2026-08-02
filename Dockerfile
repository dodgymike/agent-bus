# syntax=docker/dockerfile:1
#
# agent-bus container image -- DEPLOY-1.
#
# Two stages: a Go builder that produces a single static binary, and a
# minimal Alpine runtime that only ever runs that binary as a non-root user.
# The builder image is PINNED BY TAG AND DIGEST, not `:latest` -- an
# unpinned builder means the toolchain (and therefore the binary) can change
# out from under this Dockerfile without a single line here changing. Re-pin
# both stages' base images when DEPLOY-4 bumps go.mod's toolchain version;
# until then this MUST track go.mod's `go 1.19` line (currently go1.19.4,
# the exact patch this box's ambient `go version` reports).

# ---- Builder -------------------------------------------------------------
FROM golang:1.19.4-alpine@sha256:86d32cc0dfc04757fd8aeebb86308e6d1e3de60c73cb59e0f99c7b2ef77416b6 AS builder

WORKDIR /src

# go.mod only, no go.sum: the module has zero third-party dependencies today
# (invariant 8, stdlib-first) so there is nothing to download. If a future
# change adds a dependency, go.sum lands in the module root and MUST be
# copied here too, ahead of the full COPY below, so `go mod download` alone
# busts the layer cache instead of every source edit doing so.
COPY go.mod ./

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# ARG, not a baked-in default: passed at build time via `docker build
# --build-arg VERSION=$(git describe --tags --always)`, mirroring the
# `-ldflags "-X main.version=..."` convention documented on cmd/agent-bus's
# `version` var. Falls back to "dev" exactly like the unbuilt binary does.
ARG VERSION=dev

# CGO_ENABLED=0 for a fully static binary (no libc dependency to satisfy in
# the runtime stage). -trimpath drops host build-path strings from the
# binary; -s -w strip the symbol table and DWARF debug info, which
# `docker/dockerfile` tooling flags as an unused-info win in a shipped
# artifact but that ALSO means panics won't carry line numbers -- acceptable
# for a container binary reporting through structured stderr logs, not for a
# workstation debug build.
#
# No BuildKit cache mount here: this must build on the legacy Docker builder
# too (this box's `docker` build has no `buildx` component), and a cache
# mount is a BuildKit-only feature that fails outright without it. Layer
# caching alone (go.mod copied before the source) still avoids a from-scratch
# module fetch on unrelated source edits, which is all this module needs
# since it has no third-party dependencies to warm a build cache for.
#
# Deliberately NO explicit GOOS/GOARCH: `go build` defaults to the platform
# it is RUNNING on, which for the official `golang` image is always whatever
# platform Docker pulled (native by default, or the `--platform` requested at
# `docker build`/`buildx build` time). Hardcoding GOARCH=amd64 here would
# silently cross-compile an amd64 binary into an arm64 `alpine` runtime stage
# on an arm64 host/CI runner -- wrong architecture, and the failure mode is
# "exec format error" at container start, not a build error. Letting the
# toolchain's own default track the platform keeps the two stages consistent
# without reaching for BuildKit's TARGETARCH/TARGETPLATFORM args, which (like
# the cache mount above) are not available on the legacy builder.
RUN CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/agent-bus ./cmd/agent-bus

# ---- Runtime ---------------------------------------------------------------
# Alpine, not distroless: invariant 8 (Go stdlib first, dependencies need a
# DECISIONS.md justification -- see the 2026-08-02 DEPLOY-1 entry) extends
# here to base-image choice. distroless/static ships a marginally smaller,
# shell-less image, but this task also needs a container-local HEALTHCHECK
# (DEPLOY-2 wires a compose healthcheck against the same route), and
# distroless/static has no shell and no HTTP client to run one with -- the
# alternative is bundling a second static binary just to answer "is the
# server up", which is more moving parts for less benefit. Alpine's busybox
# ships `wget` and `adduser` out of the box, keeping both the healthcheck and
# the non-root user creation to stdlib-simple `RUN` lines, at a cost of a few
# MB over distroless. That trade is "simple beats clever" (invariant 8), not
# a shortcut around it.
FROM alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1 AS runtime

# Fixed, explicit UID/GID (not adduser's next-available default) so the
# numeric owner of the data volume is stable and predictable across image
# rebuilds -- an operator inspecting a bind-mounted volume from the host sees
# the same uid:gid every time.
RUN addgroup -g 10001 -S agentbus \
    && adduser -u 10001 -S -G agentbus -H -h /nonexistent -s /sbin/nologin agentbus

COPY --from=builder /out/agent-bus /usr/local/bin/agent-bus

# The data directory: WAL, bus id, dirlock and the MAC key (DUR-12) all live
# here (invariants 4/5/6). It is created, chowned AND chmod 0700'd to the
# non-root user BEFORE the VOLUME declaration so a fresh named volume is
# initialised from these exact permissions (Docker seeds a new volume's
# content, including ownership and mode, from whatever already exists at the
# mount path in the image) -- getting this order right is what makes a
# non-root process able to write into a volume it has never seen before. The
# explicit chmod matters on its own: `mkdir -p` alone leaves the directory at
# whatever the ambient umask gives it (typically 0755, world-readable), which
# would diverge from the 0700 contract the server code itself asserts for
# this exact directory (cmd/agent-bus/main.go's os.MkdirAll(cfg.DataDir,
# 0o700) and internal/wal's own file modes) -- nothing inside it is
# world-readable today (every file the WAL writes is already 0600), but a
# 0755 directory is still the wrong default for one holding a MAC key.
RUN mkdir -p /data && chown agentbus:agentbus /data && chmod 0700 /data
VOLUME ["/data"]

USER agentbus:agentbus
WORKDIR /

# Documentation only -- EXPOSE does not publish the port, docker-compose.yml
# / `docker run -p` does. Matches the binary's own -listen default
# (127.0.0.1:8080); see the loopback-binding note in docker-compose.yml for
# why that default must not be casually widened.
EXPOSE 8080

# Wired to the existing unauthenticated GET /healthz route -- see
# internal/httpapi/server.go. Uses the flag defaults' host:port; if an
# operator overrides -listen via CMD/command:, this must be overridden to
# match (docker-compose.yml's healthcheck does exactly that for its own
# invocation).
#
# TLS SEAM (do not implement yet -- see CLAUDE.md invariant 11 and the
# mutual-TLS epic, in_progress as of 2026-08-02): once the server serves TLS
# with a self-signed cert and no CA, plain `wget` here stops working and this
# probe must move to HTTPS with certificate material the healthcheck can
# trust. Tracked, not solved, here.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -T 2 -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/agent-bus"]
# Defaults mirror cmd/agent-bus's own flag defaults (defaultListen,
# defaultDataDir=./data resolved instead to the declared /data volume so the
# durable state always lands on the volume regardless of WORKDIR). Every flag
# here already exists on the binary (see `agent-bus -h`); nothing
# container-specific was invented. Override any of them with `docker run
# agent-bus:tag -listen ... -log-level ...` or docker-compose.yml's
# `command:`.
CMD ["-listen=127.0.0.1:8080", "-data-dir=/data", "-log-level=info"]
