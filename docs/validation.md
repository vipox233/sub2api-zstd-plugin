# Release validation

## v1.5.0

Run the same `BenchmarkOutboundPreparation` fixture on `main` at `8c7fcd0` and on the refactor. Use Go 1.27.3 on Linux amd64, `GOMAXPROCS=4` and `-benchtime=3x`. The runs below used an AMD EPYC 9V74 worker. Enable image optimization in both runs and use each version's remaining defaults. The history sample primes the first turn before measuring the next one. Other samples use a fresh optimizer for each iteration.

| Synthetic request | Original JSON | HTTP body bytes, before → after | JSON bytes saved, before → after | Local time, before → after |
|---|---:|---:|---:|---:|
| Single 4K screenshot | 17,316,446 B | 425,832 → 324,253 B | 97.54% → 98.13% | 561 → 468 ms |
| Four screenshots in history | 30,922,715 B | 12,457,724 → 1,135,586 B | 59.71% → 96.33% | 149 → 92 ms |
| Six 4K screenshots | 104,013,090 B | 78,026,822 → 1,646,737 B | 24.98% → 98.42% | 429 → 1,130 ms |
| Long text with a repeated prefix | 3,670,028 B | 2,746,214 → 2,157,889 B | 25.17% → 41.20% | 27.8 → 16.5 ms |

Local timing includes preparation and reading the body into a mock transport. At 5 Mbps, the six-image sample's payload alone would take about 125 → 2.6 seconds to send. Production first-token latency, OCR and upstream prompt-cache behavior depend on the actual requests and upstream.

To reproduce, copy `internal/plugin/bandwidth_bench_test.go` to the baseline checkout and run the same command in each checkout.

```sh
GOMAXPROCS=4 go test ./internal/plugin -run '^$' -bench '^BenchmarkOutboundPreparation$' -benchtime=3x -count=1
```

## v1.3.0

The Go and UI regression suites cover JPEG and opaque PNG, all size presets, portrait and long-image geometry, the 32 KiB boundary and zero setting, saved configuration migration, and protected-image passthrough. They also check exact JPEG recovery after restart, legacy PNG decisions saved by v1.2.x, byte-local JSON edits and the HTTP 415 retry body.

In a synthetic 8000 × 6000 JPEG probe, the 30,121,880-byte source became a 1440 × 1080 JPEG of 332,574 bytes. The final zstd body was 330,160 bytes. These figures describe the generated test image.

## v1.2.2

Go 1.27.3 on Linux amd64. The signed package passed startup, health and zstd self-test with the SDK host emulator configured for Sub2API 0.2.23. Its publisher public key matches v1.1.0.

- Go tests and vet passed. The UI contract suite passed 10 checks.
- Verified byte-local image edits, history replay and recovery after process restart with test KV and a busy encoder, session wait cancellation, 415 replay, image failure passthrough, threshold equality and fit without cropping.
- Verified SHA-256 after zstd decoding for 512 MiB under its configured limit and 513 MiB with the unlimited setting.

Synthetic screenshot probes compared final zstd payload bytes with the image stage disabled and enabled.

| Sample | zstd alone | JPEG and zstd | Reduction |
|---|---:|---:|---:|
| 2560 × 1440 panel | 5,945,210 B | 1,215,267 B | 79.6% |
| 3840 × 2160 panel | 13,325,230 B | 2,719,563 B | 79.6% |
| 3840 × 2160 color | 13,280,901 B | 2,184,330 B | 83.6% |

Reproduce with `SUB2API_IMAGE_BENCH=1 go test -mod=vendor ./internal/plugin -run '^TestImageBandwidthProbe$' -count=1 -v`. A real OAuth test is needed to measure screenshot recognition, actual time to first token and upstream cache usage.

## v1.1.0

Built on Linux amd64 using the pinned Go 1.27.3 toolchain and vendored dependencies.

- Full unit/protocol regression suite: passed.
- Go vet: passed.
- UI bridge/configuration logic: 10 tests passed.
- 512 MiB known-length body at a 512 MiB cap: compressed, decoded and SHA-256 matched.
- 513 MiB unknown-length body with max_body_bytes=0: compressed, decoded and SHA-256 matched.
- Real anonymous-file disk spooling, large 415 retry, no-gain byte preservation, cancellation, config reload and queue wake-up: passed.
- Selected concurrency/cancellation/disk tests under the Go race detector: passed.
- Built executable launched with official-v1-compatible host emulator; config, health and local forwarding: passed.
- Final signed package independently verified and extracted, then its executable started and self-tested: passed.
- Tampered UI in the signed package: rejected.

Large tests use synthetic repeated text and never allocate the full input on the plugin heap. Their compression ratios and point-in-time heap readings are not predictions for real multi-image requests or total process/container memory.

The source tree includes the complete test code and logs. UI tests are VM-based logic tests, not an authenticated real-host browser session. No authenticated OpenAI requests were sent. Production Sub2API was not modified, restarted or installed with this plugin. Host compatibility remains declared as untested; do not confuse a v1 protocol emulator with a full production-host integration test.
