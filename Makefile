# Makefile for building the agent-bus binaries.
#
# Two binaries are produced from this module (github.com/dodgymike/agent-bus):
#   - bin/agentbus    -- the bus server      (./cmd/agent-bus)
#   - bin/agentbusctl -- the bus client CLI  (./cmd/agent-busctl)
#
# Build flags mirror the container image convention in Dockerfile:
#   - CGO_ENABLED=0 for a fully static binary (stdlib-only module, invariant 8)
#   - -trimpath drops host build-path strings from the binary
#   - -ldflags "-s -w -X main.version=..." strips debug info and injects the
#     build version into cmd/agent-bus's `version` var (default "dev")
#
# bin/ is already gitignored (see .gitignore), so built artifacts never land
# in version control.

BIN_DIR    := bin
VERSION    ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
GOFLAGS    := -trimpath
CGO_ENABLED := 0

# The two binaries and the package paths they build from.
# `all` (the default target) builds both.
BINARIES := agentbus agentbusctl

.PHONY: all agentbus agentbusctl clean test vet fmt

all: $(BINARIES)

## agentbus: build the bus server binary into bin/agentbus
agentbus: $(BIN_DIR)/agentbus

## agentbusctl: build the bus client CLI binary into bin/agentbusctl
agentbusctl: $(BIN_DIR)/agentbusctl

$(BIN_DIR)/agentbus:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $@ ./cmd/agent-bus

$(BIN_DIR)/agentbusctl:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $@ ./cmd/agent-busctl

## clean: remove all built binaries
clean:
	rm -rf $(BIN_DIR)

## test: run the full test suite
test:
	go test ./...

## vet: run go vet across the module
vet:
	go vet ./...

## fmt: report files not gofmt-formatted
fmt:
	gofmt -l .
