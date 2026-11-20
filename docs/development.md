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

Image processing is independent of the zstd size limit. The scanner reads at most 64 MiB of raw request JSON in chunks and records string offsets. Image edits use at most 16 MiB of replacement URLs per request. The 32 MiB, 128-entry JPEG cache has a two-hour idle lifetime. One worker handles cold conversions, with at most two fresh conversions per request. Scanning and fresh work share a five-second cooperative budget. Restoring a recorded JPEG waits for the same worker and follows the request context. Cache hits hash the Base64 stream without allocating the decoded file.

Original-file size minus JPEG-file size must meet the configured byte floor. Zero still requires strictly smaller output. The encoder stops when the maximum acceptable JPEG length is crossed. Byte eligibility is checked against the detected format and dimensions are checked before pixel decode.

Session decisions live in up to 256 expiring slots. The slot is keyed by input history and the session header when available. A live collision falls back to original images. When provided by the host, the plugin KV stores only versioned metadata and SHA-256 digests for seven days. A restart regenerates recorded JPEGs from the original source using the stored quality and geometry and verifies each digest. Decisions from before v1.3.0 continue to use their original long-edge geometry. If recovery fails for any recorded JPEG, the image pass forwards the original body before zstd.

Build and verify against the declared 0.2.23 compatibility range with:

    make pack GO=/path/to/go PRIVATE_KEY=/private/outside/source/publisher.pem
    go run -mod=vendor ./tools/checkpackage --host-version 0.2.23 --package dist/openai-upstream-zstd-1.3.0-linux-amd64.s2plugin --public-key dist/publisher-public-key.txt

The check starts the signed executable with the SDK host emulator. Use a separate OAuth test account or isolated gateway to measure real cached tokens, OCR and time to first token.
