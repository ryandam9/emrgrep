BINARY  := bin/emrgrep
GO      := go
PKG     := ./...
MAIN    := ./cmd/emrgrep
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) \
           -X main.commit=$(COMMIT) \
           -X main.date=$(DATE)

.PHONY: all fmt vet test build install clean run tidy lint help

all: fmt vet test install

fmt:
	$(GO) fmt $(PKG)

vet:
	$(GO) vet $(PKG)

test:
	$(GO) test -race -count=1 $(PKG)

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) $(MAIN)

# install copies the binary to a bin directory (override with PREFIX=...):
#   1) ~/.local/bin, if it is on $PATH
#   2) /usr/local/bin, if it exists
install: build
	@dest="$(PREFIX)"; \
	if [ -z "$$dest" ]; then \
		case ":$$PATH:" in *":$$HOME/.local/bin:"*) dest="$$HOME/.local/bin" ;; esac; \
	fi; \
	if [ -z "$$dest" ] && [ -d /usr/local/bin ]; then dest="/usr/local/bin"; fi; \
	if [ -z "$$dest" ]; then \
		echo "install: no suitable bin dir found; rerun with PREFIX=/path/to/bin"; exit 1; \
	fi; \
	mkdir -p "$$dest" && install -m 0755 $(BINARY) "$$dest/$(notdir $(BINARY))" && \
		echo "installed -> $$dest/$(notdir $(BINARY))"

clean:
	rm -f $(BINARY)
	rm -rf bin

# run builds then runs the CLI; pass flags via ARGS, e.g.
#   make run ARGS="-bucket my-emr-logs -prefix logs/j-1ABC/ -grep ERROR"
#   make run ARGS="-bucket my-emr-logs -prefix logs/j-1ABC/ -l -discover-apps -grep 'Table not found' -F"
# Exit codes 1 (no matches) and 3 (object errors / partial scans) are
# meaningful query outcomes, so they are tolerated; anything else — 2
# (usage/config/listing failure), 130 (interrupted) — still fails the
# make invocation.
run: build
	@./$(BINARY) $(ARGS); rc=$$?; \
	if [ $$rc -eq 0 ] || [ $$rc -eq 1 ] || [ $$rc -eq 3 ]; then \
		exit 0; \
	fi; \
	exit $$rc

tidy:
	$(GO) mod tidy

lint:
ifeq (, $(shell which golangci-lint))
	@echo "golangci-lint not installed, skipping"
else
	golangci-lint run
endif

help:
	@echo "Targets:"
	@echo "  fmt      - Format source code"
	@echo "  vet      - Run go vet"
	@echo "  test     - Run tests (race detector)"
	@echo "  build    - Build binary into $(BINARY) (version/commit/date injected)"
	@echo "  install  - Build and install the binary to a bin dir (PREFIX= to override)"
	@echo "  clean    - Remove the binary and bin/ directory"
	@echo "  run      - Build and run the CLI; pass flags with ARGS=\"...\" (exit 1/3 tolerated)"
	@echo "  tidy     - Tidy go modules"
	@echo "  lint     - Run golangci-lint (if installed)"
	@echo "  all      - fmt + vet + test + install (install builds once)"
