.PHONY: test test-race fmt-check lint release clean

GO ?= go
GOCACHE ?= /tmp/go-build-cache
GOENV = env GOCACHE="$(GOCACHE)" XDG_CACHE_HOME=/tmp

test:
	mkdir -p "$(GOCACHE)"
	$(GOENV) $(GO) test ./...

test-race:
	mkdir -p "$(GOCACHE)"
	$(GOENV) $(GO) test -race ./...

fmt-check:
	@files="$$(find . -type f -name '*.go' -not -path './.git/*' -not -path './dist/*' -not -path './.agent/*')"; \
	out="$$(gofmt -l $$files)"; \
	if [ -n "$$out" ]; then \
		echo "Unformatted Go files:"; \
		echo "$$out"; \
		exit 1; \
	fi

release:
	mkdir -p "$(GOCACHE)"
	$(GOENV) $(GO) run ./cmd/release-packager -out dist

clean:
	rm -rf dist

lint:
	$(MAKE) fmt-check
	mkdir -p "$(GOCACHE)"
	$(GOENV) $(GO) vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then \
		if staticcheck -debug.version 2>/dev/null | rg -q "Compiled with Go version: go1\\.(2[6-9]|[3-9][0-9])"; then \
			$(GOENV) staticcheck ./...; \
		else \
			echo "staticcheck built with older Go toolchain; skipping"; \
		fi; \
	else \
		echo "staticcheck not installed; skipping"; \
	fi
