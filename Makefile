GO ?= go
PLUGIN_BINARY := dist/bin/linux-amd64/openai-upstream-zstd

.PHONY: build test vet ui-test vendor pack

build:
	mkdir -p dist/bin/linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false -ldflags='-s -w' -o $(PLUGIN_BINARY) ./cmd/plugin

test:
	$(GO) test -p 1 ./... -count=1 -timeout=120s

vet:
	$(GO) vet ./...

ui-test:
	node --test tests/ui-contract.test.cjs

vendor:
	$(GO) mod vendor

# Keep the private key outside this source/release directory.
pack: build
	python3 tools/package.py --binary $(PLUGIN_BINARY) --private-key $(PRIVATE_KEY) --output-dir dist
