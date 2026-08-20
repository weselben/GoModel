# Cursor sdk-bridge integration grounding (for GoModel)

Sources: [cursor/sdk-bridge README](https://github.com/cursor/sdk-bridge), [Sunnyender-org/cursor-sdk2api](https://github.com/Sunnyender-org/cursor-sdk2api). Reference for #21. The sdk-bridge README is essentially a build plan for language adapters — this note extracts the parts GoModel actually needs.

## What the bridge is

A small local process (`bin/cursor-sdk-bridge`) that embeds Cursor's TypeScript `@cursor/sdk` as a library and exposes its full surface — create/send/stream agents, custom tools, usage — over **Connect/gRPC-Web** using the versioned `sdk.v1` protobuf contract. GoModel spawns the bridge and speaks `sdk.v1` to it over a local HTTP/1.1 socket; the bridge handles all upstream Cursor backend protocol (Connect-RPC/HTTP-2 to `agentn.api5.cursor.sh`, JWT exchange, etc.).

```
GoModel cursor provider  ──spawns──►  cursor-sdk-bridge  ──HTTPS──► api5.cursor.sh
                       ◄─Connect/HTTP/1.1─
```

## What GoModel needs (vs the full SDK)

We are building a **provider**, not a public SDK — skip the public-API conveniences and custom-tools/stores callbacks. Required subset:

| Milestone | Scope | GoModel need |
| --- | --- | --- |
| 1 — Codegen | Generate Go types from `proto/sdk/v1/*.proto` (7 files, `package sdk.v1`) | **Yes** — vendor protos at a pinned tag, use `buf` with `go_package` override. |
| 2 — Bridge manager | Spawn binary, handshake, stderr-drain, shutdown | **Yes, scoped** — env `CURSOR_API_KEY` + `--workspace` + `CURSOR_SDK_CLIENT_LANGUAGE=go`; scan stderr for `cursor-sdk-bridge ready ` JSON; read bearer from `authTokenFile`; ~30 s startup timeout; clean shutdown. |
| 3 — Transport + auth + errors | Connect/HTTP/1.1 client, `Authorization: Bearer <token>` on every request (unary + streams!), decode `SdkErrorDetails` | **Yes, scoped** — `POST http://<host>:<port>/sdk.v1.<Service>/<Method>`, `application/proto` for unary, `application/connect+proto` for streaming. Bearer on **every** request (the README explicitly calls out a common bug where interceptor APIs only cover unary). |
| 4 — First turn | `SdkAgentService.CreateAgent` + `Send` (streaming) | **Yes** — `options.api_key` per-call (NOT just env — the bridge env is not a substitute on every build); discover model ids via `SdkCursorService.ListModels` (catalog calls require per-call api_key). |
| 5 — Catalog + usage | `SdkCursorService.{Me,ListModels,ListRepositories}`, `SdkAgentService.{GetUsage,...}` | **Partial** — `ListModels` (for `/v1/models`), `GetUsage` (for cost tracking). Skip the agent CRUD/list operations. |
| 6 — Custom tools / stores | Adapter-implemented callback servers | **No** — GoModel does not advertise tools back to Cursor. |

## Streaming

Server-stream envelopes, NOT SSE or plain NDJSON:

- 1-byte flags + 4-byte big-endian length frames (same wire shape as upstream `agent.v1` Connect frames per #17).
- End-of-stream flag `0x02` carries a JSON `EndStreamResponse` with any error.
- `envelope` is a oneof: ignore empty cases (keepalives); dispatch `sdk_message` on `type` (`system|assistant|tool_call|status|...`); payloads are JSON (`google.protobuf.Struct`).
- Track the last non-empty `offset`; `result` then `done` end the run.
- On stream drop: run continues; resume via `ObserveRun` + `after_offset`.

## Bridge spawn — exact contract

- Standalone archive attached to each `vX.Y.Z` release at
  `https://github.com/cursor/sdk-bridge/releases/latest`
  (`cursor-sdk-bridge-standalone-<os>-<arch>.tar.gz`,
  os `linux|darwin|win32`, arch `x64|arm64`, win32 `x64` only).
- Env: `CURSOR_API_KEY=<crsr_... key>`.
- Args: `--workspace <dir>` for local agents; client language attribution via env `CURSOR_SDK_CLIENT_LANGUAGE=go`.
- Handshake: scan stderr for literal `cursor-sdk-bridge ready `, parse JSON, validate `schemaVersion==1`, `transport=="tcp"`, `protocol=="connect"`. Apply 30 s startup timeout; on premature exit, surface captured stderr. **Never log the raw discovery line** (older bridges inline the token).
- Auth token: read from `authTokenFile` path in the ready JSON (trimmed).
- Shutdown: `SdkBridgeControlService.Shutdown` (or SIGTERM), wait ~5 s, then kill. Run on client close **and** on process exit (no orphan bridges if GoModel crashes).

## Important gotchas

- **Bearer on every request, including streams.** A common bug where Connect interceptor APIs only cover unary.
- **`options.api_key` per-call, not just env.** Without it step 4 can fail with `Invalid User API Key`.
- **`ListModels` requires per-call `api_key`** — there is no env fallback.
- **HTTP/1.1 only** — classic gRPC will not work against the bridge (it serves HTTP/1.1).
- **Drain stderr forever** — a full pipe blocks the bridge.

## Provider shape implications

A new `internal/providers/cursor` package with three concerns:

1. **Bridge manager** — spawn, handshake, shutdown. Lives once per provider instance (not per request).
2. **Transport** — thin Connect-over-HTTP/1.1 client. Reusable.
3. **Provider methods** — wrap `Client.ListModels` → `core.ModelsResponse`, `CreateAgent` + `Send` → `core.ChatResponse` / `io.ReadCloser` for streaming, `GetUsage` → usage observer input. Translate GoModel OpenAI types ↔ `sdk.v1` messages; assistant text extraction from streaming envelopes mirrors `cursor-opencode-auth`'s heuristic-string approach but now over the versioned proto contract.

Tests: bridge manager spawn tests (gated by `CURSOR_API_KEY` env + `CURSOR_SDK_BRIDGE_BIN` env override pointing to the standalone binary). Contract tests under `tests/contract/cursor_test.go` following the `kimicode_test.go` pattern, hitting a replay fixture server (we mock the bridge, not Cursor).

## License

MIT — protos vendoring and client implementation are clean.
