GO ?= go
PACKAGE_OUTPUT_DIR ?= dist
GO_MODULE_MODE ?= $(if $(wildcard vendor/modules.txt),vendor,readonly)
PLUGIN_BINARY := dist/bin/linux-amd64/openai-upstream-zstd

.PHONY: build test vet ui-test vendor pack

build:
	mkdir -p dist/bin/linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -mod=$(GO_MODULE_MODE) -trimpath -buildvcs=false -ldflags='-s -w' -o $(PLUGIN_BINARY) ./cmd/plugin

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
	test -n "$(PRIVATE_KEY)" && test -r "$(PRIVATE_KEY)"
	python3 tools/package.py --binary "$(PLUGIN_BINARY)" --private-key "$(PRIVATE_KEY)" --output-dir "$(PACKAGE_OUTPUT_DIR)"
