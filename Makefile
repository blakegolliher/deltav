# Delta-V build. `go build ./cmd/deltav` also works on its own; this file adds
# version stamping, the dv symlink, license checks and (from milestone 5) CSS.

GO       ?= go
PREFIX   ?= /usr/local
DESTDIR  ?=
BINDIR   := bin
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X deltav/internal/version.Version=$(VERSION)

# Pinned upstream Envoy release; keep in step with internal/version and README.
# The integration tests run this release binary (Linux only). The checksums
# are the SHA-256 digests GitHub publishes for the release assets.
ENVOY_VERSION := 1.40.2
ENVOY_ARCH    := $(shell uname -m | sed -e 's/^aarch64$$/aarch_64/' -e 's/^arm64$$/aarch_64/')
ENVOY_SHA256_x86_64   := 1d64506e25c1844f146162ddb2ece7010fa4e784faddb40a079021491f1f03cc
ENVOY_SHA256_aarch_64 := b9d6d1e06dd5838a3506de9f4deebad3de9a2ce6cadb87cd76222ca286213baf
ENVOY_FILE := envoy-$(ENVOY_VERSION)-linux-$(ENVOY_ARCH)
ENVOY_BIN  := .cache/envoy/$(ENVOY_FILE)
ENVOY_URL  := https://github.com/envoyproxy/envoy/releases/download/v$(ENVOY_VERSION)/$(ENVOY_FILE)

.PHONY: all build test test-short race vet licenses license-report envoy integration install uninstall clean

all: build

# bin/dvlab is the test harness (fake S3, load generator); it is not installed.
build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINDIR)/deltav ./cmd/deltav
	ln -sf deltav $(BINDIR)/dv
	$(GO) build -trimpath -o $(BINDIR)/dvlab ./cmd/dvlab

test: vet licenses
	$(GO) test ./...

test-short:
	$(GO) test -short ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

# Fails if any linked dependency has a license outside the permitted list.
licenses:
	scripts/licenses.sh check

# Regenerates THIRD_PARTY_LICENSES.md and third_party/licenses/.
license-report:
	scripts/licenses.sh report

# Fetches the pinned Envoy binary if it is missing, and verifies its checksum
# every time, so a truncated or replaced file is never run.
envoy:
	@test -n "$(ENVOY_SHA256_$(ENVOY_ARCH))" || { echo "no pinned Envoy checksum for $(ENVOY_ARCH)" >&2; exit 1; }
	@mkdir -p $(dir $(ENVOY_BIN))
	@test -f $(ENVOY_BIN) || { echo "fetching $(ENVOY_URL)" >&2; curl -fsSL -o $(ENVOY_BIN).part $(ENVOY_URL) && mv $(ENVOY_BIN).part $(ENVOY_BIN); }
	@echo "$(ENVOY_SHA256_$(ENVOY_ARCH))  $(ENVOY_BIN)" | sha256sum -c - >/dev/null || { echo "$(ENVOY_BIN): checksum mismatch; delete it and retry" >&2; exit 1; }
	@chmod +x $(ENVOY_BIN)

# Real Envoy against the daemon and fake S3 backends, all on this host.
integration: envoy
	ENVOY_BIN=$(abspath $(ENVOY_BIN)) $(GO) test -tags integration -count=1 -v ./test/integration/...

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BINDIR)/deltav $(DESTDIR)$(PREFIX)/bin/deltav
	ln -sf deltav $(DESTDIR)$(PREFIX)/bin/dv

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/deltav $(DESTDIR)$(PREFIX)/bin/dv

clean:
	rm -rf $(BINDIR)
