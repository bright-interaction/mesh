# Mesh build + install. The module is self-contained (no cgo); these targets
# build a static binary from a monorepo checkout, the sovereign install path that
# needs no published repo. `go install <module>/cmd/mesh@latest` works too once a
# repo is published at the module path (see README).
BIN ?= $(HOME)/.local/bin/mesh

# Version stamping. buildinfo.Version defaults to "dev"; stamping it here means `mesh
# version`, the web app footer and the hub's source-availability offer all report the
# commit that was actually built, instead of a hand-typed string that drifts from the
# published source. Override with `make install MESH_GIT_SHA=v0.2.0` when cutting a tag.
MESH_GIT_SHA ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS = -X github.com/bright-interaction/mesh/internal/buildinfo.Version=$(MESH_GIT_SHA)

.PHONY: install build test vet tidy fmt clean release-gates

install: ## build + install mesh to ~/.local/bin (on PATH)
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/mesh
	@echo "installed $(BIN) ($$($(BIN) --help 2>&1 | head -1))"

build: ## build to ./bin/mesh
	go build -ldflags "$(LDFLAGS)" -o bin/mesh ./cmd/mesh

release-gates: ## self-test the public-mirror redaction + build-artifact gates
	bash scripts/test-release-gates.sh

test: ## run the full test suite
	go test ./...

vet: ## go vet
	go vet ./...

fmt: ## gofmt the tree
	gofmt -w cmd internal

tidy: ## tidy go.mod
	go mod tidy

clean: ## remove build output
	rm -rf bin
