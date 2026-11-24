# make check     everything CI runs: Go format, vet, lint and tests, the client
#                is up to date, and the UI's lint, types and build
# make client    regenerate client/ after changing openapi.json
# make ui-build  build the bundle into ui/dist (the marketplace builder runs this)
#
# Requires the .core submodule, Go, Node 22.12+ and pnpm (corepack enable).

GOLANGCI_VERSION := 2.14.0
GOLANGCI_LINT ?= $(shell go env GOPATH)/bin/golangci-lint

CORE := .core
# The extension id, derived from the module path; see extensionModule in faults.go.
NAMESPACE := com.github.aux.jamiesinn.connect-otel
CLIENT_FLAGS := -module -document "$(CURDIR)/openapi.json" -namespace $(NAMESPACE) \
	-name otel-exporter-client -out "$(CURDIR)/client"

export CI ?= true

.PHONY: check go-check fmt-check vet lint test client client-check ui-check ui-build

check: go-check client-check ui-check

go-check: fmt-check vet lint test

$(CORE)/sdk/go.mod:
	@echo ".core is not checked out; run: git submodule update --init" >&2
	@exit 1

fmt-check:
	@unformatted=$$(find . -path ./.core -prune -o -path '*/node_modules' -prune -o -name '*.go' -print | xargs gofmt -l); \
	if [ -n "$$unformatted" ]; then echo "not gofmt-ed:"; echo "$$unformatted"; exit 1; fi

vet: $(CORE)/sdk/go.mod
	go vet ./...

lint: $(CORE)/sdk/go.mod
	@if ! $(GOLANGCI_LINT) version 2>/dev/null | grep -q " $(GOLANGCI_VERSION) "; then \
		echo "Installing golangci-lint v$(GOLANGCI_VERSION)..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_VERSION); \
	fi
	$(GOLANGCI_LINT) run ./...

test: $(CORE)/sdk/go.mod
	go test -race -count=1 ./...

# The @prusa packages the UI and client link to, built once in the submodule.
$(CORE)/packages/typescript/dist/index.js: | $(CORE)/sdk/go.mod
	cd $(CORE)/packages/typescript && pnpm install --frozen-lockfile && pnpm run build

$(CORE)/packages/svelte/dist/build/module.js: | $(CORE)/sdk/go.mod
	cd $(CORE)/packages/svelte && pnpm install --frozen-lockfile && pnpm run build

client/node_modules/.modules.yaml: client/package.json client/pnpm-lock.yaml $(CORE)/packages/typescript/dist/index.js
	cd client && pnpm install --frozen-lockfile

client/dist/index.js: client/node_modules/.modules.yaml $(wildcard client/src/*.ts client/src/generated/*.ts)
	cd client && pnpm run build

# The generator rewrites package.json, so pnpm needs an install before it will
# run scripts again.
client: client/node_modules/.modules.yaml
	go -C $(CORE) run ./cmd/generate-client-sdk $(CLIENT_FLAGS)
	cd client && pnpm install --frozen-lockfile && pnpm run generate:types

# Fails when openapi.json changed and `make client` wasn't run and committed.
client-check: client/node_modules/.modules.yaml
	go -C $(CORE) run ./cmd/generate-client-sdk -check $(CLIENT_FLAGS)
	cd client && pnpm run generate:types && git diff --exit-code src/generated/

ui/node_modules/.modules.yaml: ui/package.json ui/pnpm-lock.yaml client/dist/index.js $(CORE)/packages/svelte/dist/build/module.js
	cd ui && pnpm install --frozen-lockfile

ui-check: ui/node_modules/.modules.yaml
	cd ui && pnpm run check
	cd ui && pnpm run build

ui-build: ui/node_modules/.modules.yaml
	cd ui && pnpm run build
