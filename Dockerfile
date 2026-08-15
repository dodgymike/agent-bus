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

# client/ IS REQUIRED AND WAS MISSING (fixed 2026-08-15, DEPLOY-6). Its absence
# did not degrade the image, it made the image UNBUILDABLE: since RELAY-41 wired
# certificate pinning into the relay dialler, cmd/agent-bus/relaydial.go imports
# github.com/dodgymike/agent-bus/client, and the builder stage failed with
#   "no required module provides package github.com/dodgymike/agent-bus/client"
# The reason it is easy to forget is invariant 7 itself: the client package
# CANNOT live under internal/ (an agent has to be able to embed it), so it is the
# one first-party source tree that neither `cmd/` nor `internal/` covers. If a
# future package lands at the module root for the same reason, it needs a COPY
# line here too — `go build` inside this stage sees ONLY what is copied.
COPY client/ ./client/
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
#
# BOTH BINARIES ARE BUILT (2026-08-15, DEPLOY-6), and that is a requirement of
# invariant 7, not a convenience. `agent-bus` is the server; `agent-busctl` is
# THE client, and an agent never hand-writes HTTP — so an image that ships only
# the server ships a bus no agent can enrol with unless the operator also has a
# Go toolchain on the host. Shipping both makes `docker run` self-sufficient:
# the same image serves the bus AND runs `docker run --rm ... agent-busctl enrol`
# against it. They are separate `go build` invocations rather than
# `./cmd/...` because -o with a directory and a package pattern is the only
# spelling that works there, and two explicit lines read better than one clever
# one (invariant 8).
RUN CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/agent-bus ./cmd/agent-bus \
 && CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/agent-busctl ./cmd/agent-busctl

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
COPY --from=builder /out/agent-busctl /usr/local/bin/agent-busctl

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

# /identity is the AGENT-side counterpart, for `agent-busctl` (2026-08-15,
# DEPLOY-6). It is prepared for exactly the reason /data is, and getting it
# wrong fails in a way that reads like a bug in the CLI: an agent's identity
# directory holds its Ed25519 private key and its client certificate, so
# agent-busctl needs to WRITE there, and a named volume mounted onto a path
# that does not exist in the image is created root:root 0755 by Docker --
# unwritable by the non-root user this image runs as. Pre-creating it
# agentbus:agentbus 0700 means `docker run -v my-agent-id:/identity ...` just
# works, because Docker seeds a new volume's content, ownership AND mode from
# whatever is already at the mount path.
#
# Deliberately NOT declared as a VOLUME: /data is, because a bus without
# durable state is a mistake worth defaulting against, but the SERVER never
# touches /identity, and a VOLUME line here would make every plain
# `docker run agent-bus` create a stray anonymous volume it never uses.
#
# A NAMED VOLUME is also the portable answer to a trap worth stating: a host
# bind mount has to satisfy the daemon's own filesystem visibility and the
# container uid's access to the host file at once. On a snap-packaged Docker
# daemon, for instance, /tmp inside the daemon is NOT the host's /tmp, so
# `-v /tmp/whatever:/identity` mounts an EMPTY directory and the CLI correctly
# reports "no invite file at /identity/invite.json" -- a confusing error for a
# file the operator can plainly see on the host. Named volumes have no such
# ambiguity. (Observed on this box, 2026-08-15.)
RUN mkdir -p /identity && chown agentbus:agentbus /identity && chmod 0700 /identity

USER agentbus:agentbus
WORKDIR /

# Documentation only -- EXPOSE does not publish the port, docker-compose.yml
# / `docker run -p` does. Matches this image's CMD (-listen=:8080); see the
# long note above that CMD for why the CONTAINER's bind differs from the
# BINARY's loopback default, and why that is not a weakening.
EXPOSE 8080

# Wired to the existing unauthenticated GET /healthz route -- see
# internal/httpapi/server.go. Uses the flag defaults' host:port; if an
# operator overrides -listen via CMD/command:, this must be overridden to
# match (docker-compose.yml's healthcheck does exactly that for its own
# invocation), and so must -data-dir.
#
# THE TLS SEAM IS CLOSED (MTLS-LISTENER + MTLS-VERIFY, 2026-08-07). The bus now
# serves https and ONLY https, so the `wget http://...` probe that stood here
# could never succeed again and had to move in the same commit -- a container
# whose probe cannot pass is a container Docker restarts forever.
#
# It probes through the SERVER BINARY's own `healthcheck` subcommand rather
# than through busybox `wget`, and that is a deliberate choice, not a
# convenience. The bus certificate is SELF-SIGNED with no CA anywhere in the
# design, and busybox wget cannot be told to trust ONE certificate: its only
# relevant knob is --no-check-certificate, which does not verify differently,
# it does not verify at all. CLAUDE.md invariant 11 is explicit that
# certificate verification is never disabled to make something work. So the
# probe moved into the binary that is already in this image (no second
# artefact, no curl, Alpine's size argument intact) and it trusts exactly one
# root: /data/bus-tls.crt, the certificate this bus wrote itself. Being a real
# x509 verification it also checks the hostname and the VALIDITY PERIOD, so an
# expired bus certificate reports unhealthy instead of serving on quietly.
#
# See cmd/agent-bus/healthcheck.go. Exit 0 healthy, 1 unhealthy, 2 bad usage.
# Docker documents exit 2 as RESERVED, and the collision is stated rather than
# hidden: 2 is only reachable by a malformed probe invocation (a typo in the
# flags on this line or in a compose override), never by an unhealthy bus, and
# Docker treats any non-zero status as unhealthy in practice. The alternative --
# collapsing usage errors into 1 -- would make a typo here indistinguishable
# from a dead bus, which is the harder failure to debug of the two.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/agent-bus", "healthcheck", "-data-dir=/data", "-addr=127.0.0.1:8080", "-timeout=2s"]

ENTRYPOINT ["/usr/local/bin/agent-bus"]

# -----------------------------------------------------------------------------
# -listen=:8080 -- THE ONE PLACE THE CONTAINER DEVIATES FROM THE BINARY DEFAULT.
# Changed 2026-08-15 (DEPLOY-6). Read this before "restoring" 127.0.0.1.
#
# WHAT WAS WRONG. This CMD used to repeat the binary's own loopback default,
# -listen=127.0.0.1:8080. Inside a container that is not a conservative choice,
# it is a BROKEN one: the process binds the loopback interface of its OWN
# network namespace, so `docker run -p 8080:8080 agent-bus` publishes a port
# that forwards into the namespace and finds nothing listening. The bus starts,
# reports itself healthy to its own in-namespace HEALTHCHECK, and is reachable
# by nobody. `docker run -t <image>` produced a bus that could never be used.
#
# WHY FIXING IT HERE IS NOT WEAKENING INVARIANT 11. Invariant 11 keeps the
# loopback default because it "bounds exposure" -- it is a statement about
# REACHABILITY, expressed in the only isolation primitive a bare process has:
# the interface it binds. A container has a stronger primitive. The network
# NAMESPACE is the boundary, and :8080 inside an unpublished namespace is
# reachable by strictly nobody outside it -- exactly the property loopback buys
# on a host. The default in cmd/agent-bus/main.go is DELIBERATELY UNCHANGED
# (invariant 11 says it stays, and it does): a bare `agent-bus` on a host still
# binds loopback. What moved is one image's CMD, at the layer that knows it is
# running inside a namespace. See DECISIONS.md, 2026-08-15 (DEPLOY-6).
#
# WHAT THE OPERATOR NOW OWNS, STATED PLAINLY. Exposure has moved from this file
# to the `docker run` line, and it is a real transfer, not a shell game:
#   * no -p at all            -- other containers on the same user-defined
#                                docker network. This is the three-bus
#                                topology's normal case. NOT AN ISOLATION
#                                BOUNDARY ON LINUX, though: the host owns an
#                                interface on the docker bridge and routes that
#                                subnet, so any LOCAL user can reach the
#                                container's bridge address directly (verified:
#                                a TLS handshake to an unpublished container's
#                                172.20.0.2:8080 completes). -p governs reach
#                                from OFF this host, not from the host itself.
#   * -p 127.0.0.1:8080:8080  -- plus this host's loopback. The recommended
#                                spelling for a host-side agent.
#   * -p 8080:8080            -- plus EVERY interface of the host, i.e. the LAN.
#                                Docker inserts its own iptables rules, so this
#                                bypasses a host firewall that would otherwise
#                                stop it. Deliberate act; own it.
# TLS is not the mitigation being relied on here and never was: it is mandatory
# and unconditional (invariant 11, MTLS-LISTENER). What bounds exposure is the
# ENROLMENT GATE -- invite-only since 3cedcb7 (invariant 3), so an un-invited
# POST /v1/enroll is refused 403. That gate is what makes a published port
# defensible where MTLS-CLIENTAUTH alone does not: the listener only REQUESTS a
# client certificate (tls.RequestClientCert), so one that is presented
# authenticates nobody by itself. Do not read this CMD as saying the port is
# safe to expose to a hostile network; read it as saying the container is
# reachable at all, which it previously was not.
#
# AND THE GATE IS NARROWER THAN IT SOUNDS (added after the DEPLOY-6 security
# review). Invite-only enrolment bounds who can BECOME an agent. It bounds
# nothing on the routes that necessarily cannot authenticate -- enrolment,
# session begin, session complete, /healthz, /v1/info and /v1/discovery (SIX;
# internal/httpapi/authmw.go's allow-list is the authority) -- and there is NO
# RATE LIMITING on any of them (AUTH-1-FU-RATELIMIT). Two consequences are
# documented in our own code, not hypothesised: an anonymous flooder can fill
# the session table to its maximum and deny session establishment to everyone
# until entries expire (internal/auth/session.go says so, and says it must not
# be read as fixed); and handleSessionBegin answers distinguishably for an
# unknown agent, a known-but-unbound one and a bound one, which is an agent
# enumeration oracle for anyone who can reach the port. Neither is introduced
# by this change -- publishing the port is what makes them REACHABLE. See
# docs/THREE-BUS-DOCKER.md §1 before choosing a -p spelling.
#
# -data-dir=/data resolves the binary's ./data default onto the declared VOLUME
# so durable state always lands there regardless of WORKDIR. Every flag here
# already exists on the binary (see `agent-bus -h`); nothing container-specific
# was invented. Override any of them with `docker run agent-bus:tag -listen ...
# -log-level ...` or docker-compose.yml's `command:`.
# -----------------------------------------------------------------------------
CMD ["-listen=:8080", "-data-dir=/data", "-log-level=info"]
