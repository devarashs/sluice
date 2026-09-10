# sluice build tasks. The release workflow uses `dist`; the rest are for
# working locally.

BINARY      := sluice
PKG         := ./cmd/sluice
VERSION_PKG := github.com/devarashs/sluice/internal/version

# Version identity, overridable by the release workflow. Locally it comes from
# git: the nearest tag plus the short commit, or just the commit before any tag.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).Date=$(DATE)

# Platforms the release ships. Ubuntu/Debian x86_64 is the tested target;
# arm64 is cross-compiled.
PLATFORMS := linux/amd64 linux/arm64

.PHONY: build test race vet fmt lint bench capacity dist clean

build: ## Build the binary for the host platform into ./bin
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

test: ## Run the tests
	go test -count=1 ./...

race: ## Run the tests under the race detector (needs cgo)
	CGO_ENABLED=1 go test -race -count=1 ./...

vet: ## Vet the module
	go vet ./...

fmt: ## Check formatting
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

lint: fmt vet ## Formatting and vet

bench: ## Run the throughput and setup-rate benchmarks (see docs/capacity.md)
	go test -bench=. -benchmem -run=^$$ ./internal/bench/

capacity: ## Measure heap per idle connection (CONNS overrides the count)
	go run ./cmd/sluice-capacity -conns $(or $(CONNS),10000)

dist: ## Cross-compile every release platform into ./dist with a checksums file
	@rm -rf dist && mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -trimpath -ldflags "$(LDFLAGS)" -o $$out $(PKG) || exit 1; \
	done
	@cd dist && sha256sum $(BINARY)-* > checksums.txt && cat checksums.txt

clean: ## Remove build output
	rm -rf bin dist
