# Development

Use Go 1.27.3. The repository pins dependencies in go.mod and go.sum. The default build uses the checked module versions or the local vendor tree when present. Run make vendor after a clean checkout before using the offline test commands below.

```sh
GOFLAGS='-mod=vendor -p=1' go test ./... -count=1 -timeout=120s
GOFLAGS='-mod=vendor -p=1' go vet ./...
node --test tests/ui-contract.test.cjs
SUB2API_LARGE_TEST=1 GOFLAGS='-mod=vendor -p=1' go test ./internal/plugin -run '^TestLarge512MiBAndUnlimited$' -count=1 -v -timeout=180s
make build
SUB2API_TEST_PLUGIN_BINARY="$PWD/dist/bin/linux-amd64/openai-upstream-zstd" go test -mod=vendor ./internal/plugin -run '^TestPluginSubprocessOfficialV1Protocol$' -count=1
python3 tools/package.py --binary dist/bin/linux-amd64/openai-upstream-zstd --private-key /private/outside/source/publisher.pem --output-dir dist
```

Use an independent publisher key for your own builds and register its public key in the host.
Private signing keys must stay outside release artifacts and the Sub2API runtime data directory.

The request maximum is a size eligibility filter, not a preallocation size. Zero disables this filter.
Positive sizes use JSON safe integer bytes (maximum 9007199254740991); this numerical representation
bound is not a practical body-size cap. Resource capacity still applies.

Each spool holds at most 1 MiB of payload before spilling (slice allocator overhead is additional).
Zstd uses one encoder worker and a 1 MiB window. Raw and encoded bodies may both occupy temporary disk.
Source data is read in 64 KiB chunks. Known lengths are checked before upload. Unknown finite-limit
bodies preserve the already-read prefix and stream the remainder unchanged when crossing the cap.

A bodyLease owns the original and encoded spools until the forwarding method and all asynchronous
upload readers have finished. Independent SectionReader cursors avoid mutating an earlier request
when handling 415. Slots survive configuration replacement and are released on cancellation.
The queue is bounded by host/client cancellation and the host's own concurrency admission; the plugin
does not allocate another complete request body for waiting requests or retry failed network uploads.

The process's 192 MiB Go memory target is a soft GC target, not a hard memory limit and not a body-size cap.
The host can retain full request bodies independently; this plugin does not change that host behavior.

The level-3 encoder explicitly enables all-literal entropy compression, which keeps
random-looking Base64 image data eligible for alphabet-entropy savings even when
there are few dictionary matches. Incompressible data still uses no-gain bypass.

## Image optimization

The larger image-processing scope is independent of the zstd size limit. The scanner reads at most 64 MiB of raw request JSON in chunks and records string offsets. It keeps a 32 MiB, 128-entry JPEG LRU with a two-hour idle lifetime. The encoder allows one cold conversion at a time and uses a 550 ms cooperative deadline for images up to four megapixels and 1050 ms for larger images within the 12-megapixel cap. New-image work has a 1400 ms cooperative budget. Restoring a recorded JPEG has priority over fresh encodes, waits for the same single worker and follows the request context. Cache hits hash the Base64 stream without allocating the decoded PNG.

Original-file size minus JPEG-file size must pass both the configured byte floor and the rounded-up percentage floor. Setting both to zero still requires strictly smaller output. The encoder stops writing when the maximum acceptable JPEG length is crossed.

Session decisions live in up to 256 expiring slots. The slot is keyed by input history and the session header when available. A live collision falls back to original images. When provided by the host, the plugin KV stores only versioned metadata and SHA-256 digests for seven days. A restart may recover a recorded JPEG deterministically from the original source and verify its digest. If any recorded JPEG cannot be recovered, the entire image-edit pass falls back to the original body before zstd.

Build and verify against the declared 0.2.23 compatibility range with:

    make pack GO=/path/to/go PRIVATE_KEY=/private/outside/source/publisher.pem
    go run -mod=vendor ./tools/checkpackage --host-version 0.2.23 --package dist/openai-upstream-zstd-1.2.2-linux-amd64.s2plugin --public-key dist/publisher-public-key.txt

The check starts the signed executable with the SDK host emulator. Use a separate OAuth test account or isolated gateway to measure real cached tokens, OCR and time to first token.
