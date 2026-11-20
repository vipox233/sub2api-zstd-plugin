# Release validation: 1.1.0

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
