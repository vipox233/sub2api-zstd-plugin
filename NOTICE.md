# Notices and provenance

This personal plugin implements the Sub2API OpenAI OAuth outbound transport v1
contract. It is not an official Wei-Shaw or OpenAI release.

New plugin code is distributed under GNU LGPL version 3 or later. The complete
corresponding source, pinned dependency sources, build instructions, tests and
license notices are supplied in the accompanying full release ZIP. You may
modify/rebuild this plugin and the LGPL framework, and debug those modifications.
Install your rebuilt package by trusting your own publisher public key. No
publisher private key or upstream account credential is included.

## Framework and protocol

- sub2api-plugin-framework by feeeei:
  https://github.com/feeeei/sub2api-plugin-framework
- Fixed commit: d84fcd4fe7089ae61d33beac3209a2ca521d0f1a
- Go version: v0.0.0-20260924064648-d84fcd4fe708
- License: LGPL-3.0; the framework's UI Bridge file is copied unchanged.
- The framework's pluginapi package mirrors the Sub2API public protocol;
  its original upstream provenance is retained under vendor/.
- Official host contract:
  https://github.com/Wei-Shaw/sub2api/tree/v0.2.19/backend/pkg/pluginapi

## Compression design references

- Sub2API Issue #5176:
  https://github.com/Wei-Shaw/sub2api/issues/5176
- Sub2API PR #5450, commit bd05c3e6777976d814744539ad71384e28f8efcf:
  https://github.com/Wei-Shaw/sub2api/pull/5450
- OpenAI Codex request compression:
  https://github.com/openai/codex/blob/main/codex-rs/http-client/src/request.rs

The PR supplied the design direction (compress the final OAuth request body
using zstd level 3). This release has independent routing, memory budgeting,
configuration, diagnostics and transport behavior. The PR's unrelated WebSocket
TTFT changes are not included.

## Other dependencies

- github.com/klauspost/compress v1.18.2: zstd encoder/decoder.
- github.com/andybalholm/brotli v1.2.0: streaming Brotli response decoder.
- hashicorp/go-plugin, gRPC and protobuf: local host/plugin IPC.

Each dependency retains its own license and notices in vendor/. See go.mod,
go.sum and vendor/modules.txt for exact versions. Python's cryptography package
is a build-time signing dependency, not part of the running plugin.
