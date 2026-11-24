# The gate CI runs: formatting, vet, golangci-lint, and the tests under -race.
#
#   make check
#
# The Prusa Connect SDK is not published as a Go module yet, so go.mod replaces
# it with the checkout in the .core submodule. The marketplace's builder does
# not use this file; it builds the module itself (.github/workflows/release.yml).

# The golangci-lint Prusa pins for its own extensions.
GOLANGCI_VERSION := 2.14.0
GOLANGCI_LINT ?= $(shell go env GOPATH)/bin/golangci-lint

.PHONY: check fmt-check vet lint test

check: fmt-check vet lint test

.core/sdk/go.mod:
	@echo "the .core submodule is not checked out: run git submodule update --init" >&2
	@exit 1

# .core is another repository's tree, so it is left out.
fmt-check:
	@unformatted=$$(find . -path ./.core -prune -o -name '*.go' -print | xargs gofmt -l); \
	if [ -n "$$unformatted" ]; then echo "not gofmt-ed:"; echo "$$unformatted"; exit 1; fi

vet: .core/sdk/go.mod
	go vet ./...

lint: .core/sdk/go.mod
	@if ! $(GOLANGCI_LINT) version 2>/dev/null | grep -q " $(GOLANGCI_VERSION) "; then \
		echo "Installing golangci-lint v$(GOLANGCI_VERSION)..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_VERSION); \
	fi
	$(GOLANGCI_LINT) run ./...

test: .core/sdk/go.mod
	go test -race -count=1 ./...
