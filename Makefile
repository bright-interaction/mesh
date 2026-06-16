# Mesh build + install. The module is self-contained (no cgo); these targets
# build a static binary from a monorepo checkout, the sovereign install path that
# needs no published repo. `go install <module>/cmd/mesh@latest` works too once a
# repo is published at the module path (see README).
BIN ?= $(HOME)/.local/bin/mesh

.PHONY: install build test vet tidy fmt clean

install: ## build + install mesh to ~/.local/bin (on PATH)
	go build -o $(BIN) ./cmd/mesh
	@echo "installed $(BIN) ($$($(BIN) --help 2>&1 | head -1))"

build: ## build to ./bin/mesh
	go build -o bin/mesh ./cmd/mesh

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
