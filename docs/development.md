# Development

Use Go 1.27.3. The repository pins dependencies in go.mod and go.sum. The default build uses the checked module versions or the local vendor tree when present. Run make vendor after a clean checkout before using the offline test commands below.

```sh
GOFLAGS='-mod=vendor -p=1' go test ./... -count=1 -timeout=120s
GOFLAGS='-mod=vendor -p=1' go vet ./...
node --test tests/ui-contract.test.cjs
GOMAXPROCS=4 go test ./internal/plugin -run '^$' -bench '^BenchmarkOutboundPreparation$' -benchtime=3x -count=1
SUB2API_LARGE_TEST=1 GOFLAGS='-mod=vendor -p=1' go test ./internal/plugin -run '^TestLarge512MiBAndUnlimited$' -count=1 -v -timeout=180s
make build
SUB2API_TEST_PLUGIN_BINARY="$PWD/dist/bin/linux-amd64/openai-upstream-zstd" go test -mod=vendor ./internal/plugin -run '^TestPluginSubprocessOfficialV1Protocol$' -count=1
python3 tools/package.py --binary dist/bin/linux-amd64/openai-upstream-zstd --private-key /private/outside/source/publisher.pem --output-dir dist
```

Use an independent publisher key for your own builds and register its public key in the host.
Private signing keys must stay outside release artifacts and the Sub2API runtime data directory.

## Request preparation

The image limit applies to the original body. The Zstd limit applies to the body selected after image edits. Zero permits any body size. For an unknown length with only finite active limits, the plugin keeps the prefix through the larger limit and streams the rest as received when that limit is crossed.

A preparation slot covers reading, image processing and Zstd encoding for requests over the larger of 64 KiB and `min_body_bytes`, and for unknown lengths. The default is four slots. Known small bodies prepare inline. Upload and response-header waiting begin after the slot is released. The host's own request admission continues to govern concurrent uploads.

Raw and encoded buffers independently spill to anonymous files after 8 MiB. Known large bodies write directly to disk. ReadAt-based cursors support the single identity retry on HTTP 415 and a two-minute Zstd cooldown. Reader leases keep file descriptors alive while the HTTP transport is reading. A compressed request with fallback disabled releases the original body and drops its replacement references during preparation.

Zstd uses one encoder worker per request, all-literal entropy compression and the configured level and window. The default is level 5 with an 8 MiB window. The Go encoder groups the configured levels into three modes. The pool retains up to four idle encoders. The executable sets a 1 GiB soft GC target. The host manages its own request-body memory.

## Image optimization

The scanner records up to 2048 image URL spans while validating the rest of the JSON. Collected edits still apply when more image parts follow. The source SHA-256 covers decoded image bytes. The cache key also includes the effective geometry, quality, savings floor and applicable PNG background. The in-memory LRU holds up to 128 MiB of encoded URLs and 1024 entries, expiring entries after six idle hours. It stores successful replacements and deterministic skips. Concurrent misses share one encode with its own timeout and pin on the source buffer. Each request waits within its own image budget and can leave independently.

The source checks cap compressed file size, dimensions and total pixels. The JPEG encoder stops when its result would miss the configured savings floor, which defaults to 8 KiB. Edits share the encoded URL strings and are applied in original JSON byte order. An in-flight request can still hold a string after LRU eviction. A configuration or codec change can change the representation of an existing image.

## Statistics

Each logical request has one record. `final_bytes` reports bytes consumed by the transport on the last attempt. `wire_bytes` covers all attempts including portions read before a 415 response. These are HTTP body counters. Recent records contain sizes, counts, timings and skip reasons. A request-size skip is recorded at request level before scanning, so the image count is unknown for those records.

Build and verify against the declared 0.2.23 compatibility range with:

    make pack GO=/path/to/go PRIVATE_KEY=/private/outside/source/publisher.pem
    go run -mod=vendor ./tools/checkpackage --host-version 0.2.23 --package dist/openai-upstream-zstd-1.5.0-linux-amd64.s2plugin --public-key dist/publisher-public-key.txt

The check starts the signed executable with the SDK host emulator. Use a separate OAuth test account or isolated gateway to measure real cached tokens, OCR and time to first token.
