# Delta-V build. `go build ./cmd/deltav` also works on its own; this file adds
# version stamping, the dv symlink, license checks and (from milestone 5) CSS.

GO       ?= go
PREFIX   ?= /usr/local
DESTDIR  ?=
BINDIR   := bin
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X deltav/internal/version.Version=$(VERSION)

# Pinned upstream Envoy release; keep in step with internal/version and README.
ENVOY_VERSION := 1.40.2

.PHONY: all build test test-short race vet licenses license-report install uninstall clean

all: build

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINDIR)/deltav ./cmd/deltav
	ln -sf deltav $(BINDIR)/dv

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

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BINDIR)/deltav $(DESTDIR)$(PREFIX)/bin/deltav
	ln -sf deltav $(DESTDIR)$(PREFIX)/bin/dv

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/deltav $(DESTDIR)$(PREFIX)/bin/dv

clean:
	rm -rf $(BINDIR)
