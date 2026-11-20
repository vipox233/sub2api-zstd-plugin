# Development

The module and vendor tree pin the dependencies from 1.0.0. Build with Go 1.27.3.

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
