# History

## 1.5.0 - 2026-11-26

- Retire conversation slots, prefix hashing, persistent image decisions and full-body image rollback. Use a shared bounded content-addressed cache. Remove the per-request two-image and 16 MiB edit budgets.
- Make image worker count, timeout, tool resize and transparent PNG background configurable. Accept inline images in continuation requests and computer screenshot outputs, static 16-bit PNG with eXIf and primary JPEG images with MPF or trailing data.
- Default to unlimited image request size, Q78, tool fit1440, automatic workers and a 30-second budget. Keep saved size and quality overrides.
- Release preparation slots before upload. Expose Zstd level/window with defaults 5/8 MiB and spill buffers after 8 MiB.
- Add a 30-request size and timing history, visible skip reasons and per-attempt byte accounting for 415. Cover the transport and UI changes with regression tests and reproducible bandwidth benchmarks.

## 1.4.0 - 2026-11-25

- Make the image rewrite request-size limit configurable independently of zstd, with 64 MiB as the default and zero for unlimited.
- Allow positive integer compression concurrency settings above four. Existing saved values carry over.
- Report average image processing time per request with replacements and the byte savings across replaced images.
- Cover independent limits and unlimited settings for requests with and without Content-Length.

## 1.3.0 - 2026-11-20

- Optimize inline JPEG and opaque static PNG. Add 1080p, 1440p and original-dimension presets, portrait fit and long-image handling.
- Replace the v1.2.x two-part savings gate with one configurable byte floor. Migrate saved settings and reproduce earlier JPEG decisions with their stored geometry.
- Cover large and protected images, preset defaults, exact savings thresholds, cache-empty recovery and zstd 415 replay in the regression tests.

## 1.2.2 - 2026-11-20

- Cancel image session-slot waits when their request ends and release the forwarding reservation promptly.

## 1.2.1 - 2026-11-20

- Wait for the bounded encoder when restoring a recorded JPEG after eviction or restart. Discard the image edits together if a recorded representation cannot be recovered.

## 1.2.0 - 2026-11-20

- Added bounded PNG-to-JPEG optimization before the existing zstd HTTP transport.
- Kept existing zstd configuration defaults and the publisher identity. Image optimization is opt-in.
- Added byte-exact JSON patching, persistent decision metadata when available, and image and wire-size stats.

## 1.1.0 - 2026-11-04

Requested: remove the restrictive 64 MiB request/aggregate cap for a bandwidth-limited server.
Implemented: 512 MiB default; 0 unlimited; custom positive sizes; no aggregate multiplication cap;
bounded-chunk compression and anonymous temporary spools; wait rather than bypass compression when busy.
Old saved configuration remains unchanged until the administrator saves a new size.
Same plugin ID, new release version and publisher key ID vipox-zstd-20261104.
Built and tested in an isolated server workspace. No production installation or authenticated OpenAI requests.
Tests and exact artifact hashes are recorded in validation.md and the accompanying release files.

## 1.0.0 - 2026-11-03

Initial OAuth outbound transport release, with a 16 MiB default and 64 MiB cap.
