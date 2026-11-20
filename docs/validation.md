# Release validation

## v1.2.1

Go 1.27.3 on Linux amd64. The signed package passed startup, health and zstd self-test with the SDK host emulator configured for Sub2API 0.2.23. Its publisher public key matches v1.1.0.

- Go tests and vet passed. The UI contract suite passed 10 checks.
- Verified byte-local image edits, history replay and recovery after process restart with test KV and a busy encoder, 415 replay, image failure passthrough, threshold equality and fit without cropping.
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
