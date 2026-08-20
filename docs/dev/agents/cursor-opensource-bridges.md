# OSS survey: Cursor subscription → OpenAI-compatible bridges

**Ticket:** weselben/GoModel#18 — *Research: open-source cursor-cli → OpenAI-compatible bridges + wrapper authentication*
**Author:** research subagent (forge / wayfinder), 2026-08-20
**Scope:** survey the open-source landscape; identify the 2–4 most credible projects and what GoModel should mirror; flag ToS / maintenance / license risks. Companion to #17 (Cursor auth/protocol internals).

---

## TL;DR

Two distinct design patterns have emerged in the OSS ecosystem, plus one official one:

1. **CLI-subprocess wrappers** (the dominant pattern) — spawn `cursor-agent` as a child process and translate its `stream-json` stdout to OpenAI SSE. Mirrors upstream PR ENTERPILOT/GoModel#706's Claude Code approach almost exactly. Examples: `tageecc/cursor-agent-api-proxy`, `anyrobert/cursor-api-proxy`, `Azhi-ss/cursorcli2api`, `mhrmsg/cursor-pro-account2api`, `netandreus/pi-cursor-provider`.
2. **Reverse-engineered direct gRPC** — speak Cursor's protobuf-over-HTTP/2 agent protocol (`api2.cursor.sh/agent.v1.AgentService/...`) directly, with PKCE-OAuth access tokens. Examples: `wisdgod/cursor-api` (archived), `egoist/cursor-openai-api`. Fragile; the `wisdgod` author archived the repo citing *"frequent upstream API breaking changes led to fragile workarounds"*.
3. **Official `@cursor/sdk` + `cursor-sdk-bridge`** — Cursor itself ships a TypeScript SDK and a `sdk.v1` Connect/gRPC-Web bridge designed for adapters in *any* language (`cursor/sdk-bridge`, MIT, 73★, Cursor's own GitHub org). `Sunnyender-org/cursor-sdk2api` builds a full Anthropic/OpenAI/Responses gateway on top of it. This is the ToS-clean path; it does not reverse-engineer anything and does not require the subscription OAuth login (uses a Cursor *User API Key* from `cursor.com/settings`).

**Recommendation for GoModel:** mirror the CLI-subprocess pattern from `tageecc/cursor-agent-api-proxy` for the *subscription* (Pro/Ultra OAuth) path, and the `cursor/sdk-bridge` + `@cursor/sdk` model for the *User-API-Key* path if/when we want a metered/safer path. Skip the reverse-engineered approach — it is brittle and a Cursor ToS §1.5(i)/(vi) risk.

---

## Summary table

Sorted by relevance to GoModel. Stars/license/recency captured 2026-08-20 via `gh repo view` / GitHub REST.

| Project | Stars | License | Last push | Auth pattern | Backend pattern | Status | Verdict |
|---|---|---|---|---|---|---|---|
| [cursor/sdk-bridge](https://github.com/cursor/sdk-bridge) | 73 | MIT | 2026-08-13 | Cursor User API Key (`CURSOR_API_KEY`) | Official `sdk.v1` Connect/gRPC bridge, spawns `@cursor/sdk` locally | **Official**, Cursor-maintained | **Mirror.** Stable protobuf contract regenerated per SDK release; explicit adapter-guide for Go/Rust/Java. |
| [Sunnyender-org/cursor-sdk2api](https://github.com/Sunnyender-org/cursor-sdk2api) | 16 | MIT | 2026-08-19 | Cursor User API Key + account pool | Wraps official `@cursor/sdk` | Active | **Mirror.** Full Anthropic / OpenAI / Responses gateway over official SDK; account pool + dashboard; exposes Cursor-routed Grok via `/v1/responses`. |
| [tageecc/cursor-agent-api-proxy](https://github.com/tageecc/cursor-agent-api-proxy) | 55 | MIT | 2026-04-03 | `agent login` (subscription) or `CURSOR_API_KEY` | CLI subprocess, `stream-json` parse | Mature, npm-published | **Mirror (subscription path).** Canonical `agent -p --output-format stream-json --stream-partial-output --yolo --model X` invocation. |
| [anyrobert/cursor-api-proxy](https://github.com/anyrobert/cursor-api-proxy) | 202 | none declared | 2026-08-19 | CLI subprocess via `cursor-agent`/`agent`; keychain token cache | CLI subprocess (ACP + stream-json) | **Very active** | **Study.** Richest CLI-wrapper feature set: TLS, workspace isolation, account pool, session logs, ACP tool passthrough. No declared license — verify before reuse. |
| [egoist/cursor-openai-api](https://github.com/egoist/cursor-openai-api) | 34 | MIT | 2026-03-20 | PKCE OAuth (`cursor.com/loginDeepControl` → `api2.cursor.sh/auth/poll`) | Reverse-engineered protobuf over HTTP/2 (`api2.cursor.sh/agent.v1.AgentService/Run`) | Maintained | **Study only.** Reference for direct-backend proto headers and PKCE flow, but fragile and ToS §1.5(i) risk. |
| [Azhi-ss/cursorcli2api](https://github.com/Azhi-ss/cursorcli2api) | 70 | none declared | 2026-05-12 | CLI subprocess | Multi-CLI gateway (Cursor + Codex + Claude + Gemini) | Active | **Reference.** Shows the multi-CLI-wrapper abstraction (Hono server + per-CLI adapter). |
| [mhrmsg/cursor-pro-account2api](https://github.com/mhrmsg/cursor-pro-account2api) | 1 | MIT | 2026-03-11 | CLI subprocess | `cursor-agent --model X --print` | Small | **Reference.** Minimal CLI-wrapper, useful as a stripped-down baseline. |
| [netandreus/pi-cursor-provider](https://github.com/netandreus/pi-cursor-provider) | n/a | – | 2026-02-19 | CLI subprocess (`agent --api-key`) | CLI subprocess for Pi Coding Agent | Active | **Reference.** Confirms `agent --api-key` flag for passing the Cursor API key per-call. |
| [wisdgod/cursor-api](https://github.com/wisdgod/cursor-api) | 693 | NOASSERTION | 2026-04-01 | Browser cookie `WorkosCursorSessionToken` | Reverse-engineered backend | **Archived** | **Do not use.** Archived by author citing breakage. NOASSERTION license is a viral-leak risk. |
| [7836246/cursor2api](https://github.com/7836246/cursor2api) | 1883 | MIT | 2026-06-01 | Browser cookie | Proxies the **Cursor Web Docs free chat** endpoint, not subscription | Active but **dead-end for our use case** | **Not applicable.** As of 2026-04-01 the upstream it depends on only serves `gemini-3-flash` (author's own note: *"Cursor文档页仅剩gemini-3-flash （凉）"*). Targets a different product surface. |
| [NGLSG/Cursor2API](https://github.com/NGLSG/Cursor2API) | 52 | MIT | 2026-08-18 | Cursor API Key (`crsr_…`) | Local-sidecar gateway; uses `@cursor/sdk` bridge | Active | **Reference.** Already a 3-protocol gateway (Responses/Messages/Chat); worth comparing to `cursor-sdk2api`. |
| [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) | 48 055 | Apache-2.0 | active | CLI subprocess (Antigravity/Codex/Claude/Gemini) | Multi-CLI gateway | Active | **Negative finding.** Despite its name and Go language, the project does **not** ship a Cursor executor (tree search for `cursor` returns only ecosystem references). Our Go-comparable reference for the multi-CLI gateway pattern, but no direct reuse for Cursor. |

> **License watch:** MIT and Apache-2.0 are both GoModel-compatible. `anyrobert/cursor-api-proxy`, `Azhi-ss/cursorcli2api`, and `wisdgod/cursor-api` either declare no license or NOASSERTION — copy no code from them without a license confirmation or rewrite.

---

## Per-project detail

### 1. `cursor/sdk-bridge` — Cursor's official adapter bridge  *(recommended)*

- Repo: <https://github.com/cursor/sdk-bridge> — Cursor's own GitHub org, MIT, 73★, last pushed 2026-08-13.
- The README describes it as *"the public home of the Cursor SDK bridge protocol: the stable `sdk.v1` protobuf contract that lets you drive Cursor agents from any language, without depending on the TypeScript (`@cursor/sdk`) or Python (`cursor-sdk`) SDKs directly."*
- Architecture: a small local server that embeds `@cursor/sdk` as a library and exposes its full surface over [Connect](https://connectrpc.com/)/gRPC-Web using protos in `proto/sdk/v1/`. An *adapter* spawns the bridge and speaks `sdk.v1` to it.
- Cursor's README explicitly invites non-TS adapters: *"Building an adapter for a new language? Give a Cursor agent this repository and the Agent: start here guide."* — direct relevance for a Go adapter.
- Distribution: prebuilt standalone binaries `cursor-sdk-bridge-standalone-{linux|darwin|win32}-{x64|arm64}.tar.gz` attached to every GitHub release, plus `SHA256SUMS.txt`.
- **Two auth domains** (per [`docs/protocol.md`](https://github.com/cursor/sdk-bridge/blob/main/docs/protocol.md)):
  1. **Bridge auth** — per-process bearer token generated on launch, written to `authTokenFile` (mode `0600`). Read and send as `Authorization: Bearer <token>` on every RPC.
  2. **Cursor auth** — `CURSOR_API_KEY`, set explicitly on `AgentOptions.api_key` (`CreateAgent`/`ResumeAgent`) and on every `CursorRequestOptions.api_key`. Env var alone is *not* sufficient on every build.
- Lifecycle: spawn the bridge, parse stderr for the line `cursor-sdk-bridge ready {"schemaVersion":1,...,"url":"http://127.0.0.1:49152","authTokenFile":"...","protocol":"connect"}`, then Connect over loopback TCP.
- **Versioning promise**: protocol tags match `@cursor/sdk` npm versions; the `sdk.v1` package is the stable contract regenerated automatically on every SDK release ([source](https://github.com/cursor/sdk-bridge)).

**What GoModel mirrors:**
- Spawn the standalone bridge binary as a subprocess (managed lifecycle, stderr-parsed ready line, 30 s startup timeout per the reference adapters).
- Speak `sdk.v1` Connect/gRPC-Web over loopback with the bearer token.
- Pass the `CURSOR_API_KEY` explicitly on every `CreateAgent` call — do not rely on env alone.
- Pull `proto/sdk/v1/*.proto` via `buf generate` (or hand-roll) into Go types; pin a bridge release tag.

### 2. `Sunnyender-org/cursor-sdk2api` — full gateway on top of the official SDK  *(recommended reference)*

- Repo: <https://github.com/Sunnyender-org/cursor-sdk2api> — MIT, 16★, last pushed 2026-08-19 (very fresh).
- *Description:* "Official Cursor SDK to Anthropic-compatible HTTP gateway with native MCP tool continuation." Wraps the published `@cursor/sdk`; uses the official Cursor Agent harness, not browser cookies or private transports.
- Exposes `/v1/messages` (Anthropic), `/v1/chat/completions` (OpenAI), `/v1/responses` (OpenAI Responses), `/v1/models`, plus a `/console/` dashboard.
- **Grok support** is explicit: *"Grok Build via `/v1/responses`: streaming, function tools, continuation, reasoning usage"* — confirming that Cursor-routed Grok (including the variants bundled with Cursor subscriptions) is reachable through the SDK path. *Caveat the README adds:* "Cursor-routed Grok does not provide xAI-native `x_search`."
- Account pool: persistent Cursor account pool with model-aware round-robin, stale SDK-auth recovery, pre-semantic account failover, dashboard quota, Docker.
- 1M-context forwarding: when Cursor's live catalog exposes `context=1m` (Sonnet 4.6, Fable 5), forwards the SDK parameter unchanged.

**What GoModel mirrors:**
- Three-protocol surface (Messages/Chat/Responses) — pick the one closest to GoModel's canonical OpenAI pipeline first.
- Account pool model (round-robin + failover) — GoModel already has keyring + failover; this just confirms the design point.
- The `dashboard/`-style quota view — useful as a UX prior.

### 3. `tageecc/cursor-agent-api-proxy` — canonical CLI subprocess wrapper  *(recommended for the subscription path)*

- Repo: <https://github.com/tageecc/cursor-agent-api-proxy> — MIT, 55★, npm-published (`cursor-agent-api-proxy`), last pushed 2026-04-03.
- The README is unambiguous: *"OpenAI-compatible API proxy for the Cursor CLI. Lets any OpenAI client use your Cursor subscription."*
- Auth: `agent login` (browser OAuth, the subscription path) — *or* `CURSOR_API_KEY` env var for headless / API-key path.
- Surface: `GET /v1/models`, `POST /v1/chat/completions` (streaming), `GET /health`. Listens on `:4646`.
- Model IDs are taken verbatim from `agent --list-models` (e.g. `gpt-5.2`, `gpt-5.3-codex`, `opus-4.6-thinking`, `sonnet-4.5-thinking`, `gemini-3-pro`, `auto`). No re-mapping — the proxy is transparent.
- The canonical subprocess invocation lives in [`src/subprocess/manager.ts`](https://github.com/tageecc/cursor-agent-api-proxy/tree/main/src/subprocess/manager.ts):

  ```
  spawn("agent", [
    "-p",                                  // print mode
    "--output-format", "stream-json",      // NDJSON assistant deltas
    "--stream-partial-output",             // stream deltas as they arrive
    "--yolo",                              // auto-approve tool calls
    "--model", "<model-id>",               // optional
    <prompt-stdin-or-arg>,
  ])
  ```

- Background mode: registered as a system service (`launchd` on macOS, Task Scheduler on Windows, systemd user service on Linux) — same idea as a daemonized Go subprocess.

**What GoModel mirrors:**
- The exact subprocess argv pattern. Postel's-law-friendly: translate OpenAI `messages` → a single `--prompt` payload (or stdin), drop `tools` (CLI has its own tool loop), preserve `stream` by reading NDJSON from stdout.
- Model list discovery: `agent --list-models` — wrap it for `/v1/models` instead of trusting a hardcoded list.
- Auth detection: prefer `agent status` to read cached token; fall back to `CURSOR_API_KEY` env for headless deployments.
- Streaming: line-delimited JSON of shape `{"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}` then a final `{"type":"result","subtype":"success"}` — see [the anyrobert parser](https://github.com/anyrobert/cursor-api-proxy/blob/main/src/lib/cli-stream-parser.ts).

### 4. `anyrobert/cursor-api-proxy` — richest CLI wrapper  *(study for features)*

- Repo: <https://github.com/anyrobert/cursor-api-proxy> — 202★, no license declared, last pushed 2026-08-19 (most actively maintained of the bunch).
- *Description:* "OpenAI-compatible proxy for Cursor CLI. Run it on localhost and point any LLM client at it like a normal chat API."
- Subprocess layer (`src/lib/agent-runner.ts`): prefers `cursor-agent` on `PATH`, falls back to `agent`; `CURSOR_AGENT_BIN` override for vendor collisions.
- Two CLI modes:
  - **stream-json** (default): `agent -p --output-format stream-json --stream-partial-output --yolo ...` — parsed by `src/lib/cli-stream-parser.ts`, emitting incremental OpenAI SSE.
  - **ACP** (Agent Client Protocol): `cursor-agent acp --workspace ... --model ...` — JSON-RPC over stdio. Lets the proxy inject client-side tools; useful for MCP-tool continuation.
- Workspace isolation: `CURSOR_BRIDGE_CHAT_ONLY_WORKSPACE=true` runs the CLI in an empty temp dir; the proxy overrides `HOME`, `USERPROFILE`, and `CURSOR_CONFIG_DIR` so global rules don't leak. Optional `X-Cursor-Workspace` header per request.
- TLS via Tailscale certs (`CURSOR_BRIDGE_TLS_CERT` / `CURSOR_BRIDGE_TLS_KEY`).
- Token cache: writes the keychain token into a per-account cache file (`src/lib/token-cache.ts`).
- Account pool (`src/lib/account-pool.ts`), admin dashboard (`src/lib/admin-dashboard.ts`), CLI for inspecting completed requests (`cursor-api-proxy requests --watch`).

**What GoModel mirrors (conceptually):**
- The two-mode CLI invocation (stream-json vs. ACP). For GoModel's first cut, stream-json is enough; ACP becomes interesting if we want to forward tool calls back to the caller.
- Workspace isolation toggle — important to avoid leaking the gateway host's filesystem into the agent.
- The "prefer `cursor-agent`, fall back to `agent`" precedence.
- *License caveat:* no `LICENSE` file in the repo at survey time — copy patterns, not code.

### 5. `egoist/cursor-openai-api` — reverse-engineered direct gRPC  *(study for the protocol, do not reuse as a backend)*

- Repo: <https://github.com/egoist/cursor-openai-api> — MIT, 34★, last pushed 2026-03-20.
- Speaks Cursor's protobuf-over-HTTP/2 agent protocol directly against `https://api2.cursor.sh/agent.v1.AgentService/Run`, with full protobuf bindings in [`src/proto/agent_pb.ts`](https://github.com/egoist/cursor-openai-api/tree/main/src/proto/agent_pb.ts).
- **PKCE OAuth flow** ([`src/auth.ts`](https://github.com/egoist/cursor-openai-api/blob/main/src/auth.ts)):
  - `https://cursor.com/loginDeepControl?challenge=<pkce>&uuid=<uuid>&mode=login&redirectTarget=cli` — opens browser.
  - Polls `https://api2.cursor.sh/auth/poll?uuid=<uuid>&verifier=<pkce>` every 1–10 s (exponential backoff, 150 attempts).
  - Refresh via `POST https://api2.cursor.sh/auth/exchange_user_api_key` with `Authorization: Bearer <refresh_token>`, body `{}`.
- **HTTP/2 transport** ([`src/h2-bridge.mjs`](https://github.com/egoist/cursor-openai-api/blob/main/src/h2-bridge.mjs) + [`src/models.ts`](https://github.com/egoist/cursor-openai-api/blob/main/src/models.ts)): a Node subprocess running `node:http2` (Bun's `node:http2` is broken). Request headers (verbatim, via `curl --http2`):
  ```
  content-type: application/proto
  te: trailers
  authorization: Bearer <access-token>
  x-ghost-mode: true
  x-cursor-client-version: cli-2026.02.13-41ac335
  x-cursor-client-type: cli
  ```
  Endpoints: `/agent.v1.AgentService/Run` (chat) and `/agent.v1.AgentService/GetUsableModels` (model discovery).
- Model fallback (hardcoded) when `/GetUsableModels` is unreachable: `composer-2`, `claude-4-sonnet`, `claude-3.5-sonnet`, `gpt-4o`, `cursor-small`, `gemini-2.5-pro`.
- **Why study, not mirror:** the `wisdgod` archive note and the fact that `egoist` itself only ships a `composer-2`-centric schema (no Grok IDs, no `opus-4.6-thinking`, etc.) both point at the same fragility — Cursor ships new model IDs and protobuf revisions faster than OSS reverse engineers can keep up. It is also the path Cursor's ToS §1.5(i) ("reverse engineer ... or attempt to derive or gain access to the source code, object code or underlying structure of the Service") and §1.5(vi) ("probe, scan or attempt to penetrate the Service") are most naturally read to prohibit.

### 6. `Azhi-ss/cursorcli2api` — multi-CLI gateway framework  *(reference)*

- Repo: <https://github.com/Azhi-ss/cursorcli2api> — 70★, no license, last pushed 2026-05-12.
- Single Hono server fronts *Cursor Agent + Codex + Claude Code + Gemini CLI* behind `/v1/chat/completions`, `/v1/responses`, `/v1/messages`. Each request `child_process.spawn`s the relevant CLI and parses its NDJSON output.
- Exposes OpenAI + Anthropic + Responses from one binary; concurrency-controlled by a semaphore; SIGTERM/SIGINT cleanly drains children.

**What GoModel mirrors:**
- The idea that one provider family (CLI-wrapped subscriptions) is a *category*, not a single provider — same as GoModel's existing `kimicode` pattern.
- The semaphore/concurrency control idea (one CLI per slot, queue when busy).

### 7. `wisdgod/cursor-api` — historical reference (do not reuse)

- Repo: <https://github.com/wisdgod/cursor-api> — 693★, `NOASSERTION` license, **archived**.
- README's first line: *"⚠️ This branch is archived and will no longer be maintained."* Archive reason (verbatim from the README): *"Frequent upstream API breaking changes led to fragile workarounds"* and *"Project structure no longer sustainable for further development"*.
- Auth: extract `WorkosCursorSessionToken` cookie from the Cursor web app (the third colon-separated field after URL-decoding `%3A%3A` → `::`), send as Bearer to Cursor's reverse-engineered backend. This is the "browser cookie" approach.
- License `NOASSERTION` means the author has not granted reuse rights under a standard OSS license — copy no code.

### 8. `7836246/cursor2api` — high stars, wrong product  *(not applicable)*

- Repo: <https://github.com/7836246/cursor2api> — 1883★, MIT, last pushed 2026-06-01.
- Despite the name, this proxies the **Cursor Web Docs free AI chat** endpoint (`/api/chat` on `cursor.com`'s public docs page) — *not* the subscription-bundled agent API. The author's own changelog: *"20260401 Cursor文档页仅剩gemini-3-flash （凉）"* — as of 2026-04-01 the docs-page chat has been throttled down to one model. It is a brilliant piece of work for the Web Docs product but is the wrong product for our subscription use case. Listed for completeness because it tops the GitHub search.

### 9. `NGLSG/Cursor2API` — active local gateway over the Cursor SDK

- Repo: <https://github.com/NGLSG/Cursor2API> — 52★, MIT, last pushed 2026-08-18.
- "Light gateway for Cursor Composer. OpenAI / Anthropic compatible API. Responses / Messages / Chat Completions. Accepts `crsr_…` API keys."
- Already ships Responses + Messages + Chat Completions in one binary. Uses `@cursor/sdk` Bridge locally (`scripts/cursor-sdk-local-agent-bridge.mjs`).
- Useful comparison point to `Sunnyender-org/cursor-sdk2api` — both reach the same backend but the UX surfaces differ.

### 10. `router-for-me/CLIProxyAPI` — negative finding

- Repo: <https://github.com/router-for-me/CLIProxyAPI> — 48 055★, Apache-2.0, very active, written in Go.
- *"Wrap Antigravity, ChatGPT Codex, Claude Code, Grok Build as an OpenAI/Gemini/Claude/Codex compatible API service."*
- Despite the scale and the Go language (our natural comparator), **the repo does not ship a Cursor executor.** Tree search for `cursor` returns only ecosystem references (Quotio, claude-dialects, 9Router). Their README explicitly enumerates: Antigravity, ChatGPT Codex, Claude Code, Grok Build — no Cursor. Worth noting because it means (a) there is no large Go reference for the CLI-wrapper pattern we would have to mirror, and (b) upstreaming a GoModel cursor provider back to that project is unlikely to be a short conversation (a precedent for Anthropic's April 2026 third-party-harness ban makes this a sensitive area).

---

## What GoModel should mirror

Two concrete paths. The map should pick one (or treat them as two providers).

### Path A — CLI subprocess wrapper  *(mirrors tageecc, simpler, no SDK download)*

This is the path closest to upstream PR ENTERPILOT/GoModel#706's Claude Code approach and to in-tree `internal/providers/kimicode`.

| Concern | Recommendation | Source |
|---|---|---|
| Subprocess invocation | `exec.Command("agent", "-p", "--output-format", "stream-json", "--stream-partial-output", "--yolo", "--model", modelID)` | [tageecc/src/subprocess/manager.ts](https://github.com/tageecc/cursor-agent-api-proxy/blob/main/src/subprocess/manager.ts#L121-L133) |
| Binary resolution | Try `cursor-agent` first, fall back to `agent`; honour `CURSOR_AGENT_BIN` for vendor-collision cases | [anyrobert/src/lib/agent-runner.ts](https://github.com/anyrobert/cursor-api-proxy/blob/main/src/lib/agent-runner.ts) |
| Auth | `agent login` OAuth OR `CURSOR_API_KEY` env (CLI accepts `--api-key`) | [tageecc README](https://github.com/tageecc/cursor-agent-api-proxy#prerequisites), [netandreus/pi-cursor-provider](https://github.com/netandreus/pi-cursor-provider) |
| Model discovery | `agent --list-models` for the `/v1/models` endpoint; fall back to a hardcoded list mirroring tageecc's set if the binary is absent | [tageecc README](https://github.com/tageecc/cursor-agent-api-proxy#models) |
| Streaming | Read line-delimited JSON from stdout: `{"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}` for deltas, `{"type":"result","subtype":"success"}` for terminator | [anyrobert/src/lib/cli-stream-parser.ts](https://github.com/anyrobert/cursor-api-proxy/blob/main/src/lib/cli-stream-parser.ts) |
| Workspace isolation | Run in an empty temp dir by default; override per request if the caller sets a `X-Cursor-Workspace` header. Override `HOME` and `CURSOR_CONFIG_DIR` so global rules don't leak | [anyrobert README §Local workspace and agent frameworks](https://github.com/anyrobert/cursor-api-proxy#local-workspace-and-agent-frameworks) |
| Cancellation | Close the subprocess on client disconnect (`context.Cancel`-aware `cmd.Process.Kill()`) | [Azhi-ss/cursorcli2api README](https://github.com/Azhi-ss/cursorcli2api) (it auto-kills children on client disconnect) |
| Concurrency | One CLI slot per request; semaphore-bounded to avoid spawning unbounded subprocesses | [Azhi-ss/cursorcli2api](https://github.com/Azhi-ss/cursorcli2api) |
| Tool calls | **Lossy** — CLI has its own tool loop; surface as opaque `reasoning_content` / informational events, do *not* try to forward them as OpenAI `tool_calls` (CLI consumes them internally). If we ever need client-side tools, switch to ACP mode like anyrobert does | [anyrobert/src/lib/acp-client.ts](https://github.com/anyrobert/cursor-api-proxy/tree/main/src/lib/acp-client.ts) |
| Usage / cost | Cursor bundles Grok + Claude + GPT behind one subscription; cost display is "subscription" mode (zero or list-price, configurable). Streaming SSE has no per-token usage chunk — fall back to `agent usage` CLI subcommand for periodic snapshots | [tageecc README](https://github.com/tageecc/cursor-agent-api-proxy) (no usage plumbing), [NetAndreus README](https://github.com/netandreus/pi-cursor-provider) |

### Path B — Official `cursor-sdk-bridge` adapter  *(ToS-clean, but heavier and uses User API Key)*

Use this path if/when we want a metered key path (Team / service-account) or want to keep GoModel's dependency surface minimal of subprocess management.

| Concern | Recommendation | Source |
|---|---|---|
| Binary | Download `cursor-sdk-bridge-standalone-linux-{x64\|arm64}.tar.gz` from the [latest release](https://github.com/cursor/sdk-bridge/releases/latest), verify against `SHA256SUMS.txt`, embed in the provider's install step (or download on first use) | [cursor/sdk-bridge README](https://github.com/cursor/sdk-bridge#getting-the-bridge) |
| Spawn | `cmd := exec.CommandContext(ctx, "cursor-sdk-bridge")`, `CURSOR_API_KEY` in env, **capture stderr**, parse the `cursor-sdk-bridge ready {"schemaVersion":1,...}` line | [cursor/sdk-bridge docs/protocol.md](https://github.com/cursor/sdk-bridge/blob/main/docs/protocol.md) |
| RPC client | Use a generated Connect/gRPC-Web client from `proto/sdk/v1/*.proto` via `buf generate`. Pin a release tag (`v1.0.0`, etc.) so the proto/Go code stays in lockstep with the bridge binary | [cursor/sdk-bridge docs/versioning.md](https://github.com/cursor/sdk-bridge/blob/main/docs/versioning.md) |
| Auth | Read `authTokenFile`, send `Authorization: Bearer <token>` on every RPC; set `AgentOptions.api_key = $CURSOR_API_KEY` explicitly on `CreateAgent` | [cursor/sdk-bridge docs/protocol.md](https://github.com/cursor/sdk-bridge/blob/main/docs/protocol.md) (two auth domains) |
| Streaming | The bridge exposes run stream envelopes with offsets/resume/keepalive — wire those into GoModel's SSE pipeline | [cursor/sdk-bridge docs/streaming.md](https://github.com/cursor/sdk-bridge/blob/main/docs/streaming.md) |
| Reference gateway | [`Sunnyender-org/cursor-sdk2api`](https://github.com/Sunnyender-org/cursor-sdk2api) already implements Responses/Messages/Chat Completions over this stack — pattern, not code, since it's TS | [Sunnyender-org/cursor-sdk2api README](https://github.com/Sunnyender-org/cursor-sdk2api) |

**Hybrid recommendation.** Default provider on Path A (CLI subprocess, uses the user's `agent login` subscription). Keep Path B as a v2 option behind a config flag (`cursor.use_bridge: true`) for users with Team / service-account keys and for the ToS-conservative case. Both can share the same `internal/providers/cursor` skeleton; the auth + transport layer is the only thing that swaps.

---

## Risks

### ToS / compliance

- **Cursor ToS §1.5(i)** ([cursor.com/terms-of-service](https://cursor.com/terms-of-service)) prohibits *"reverse engineer, disassemble, decompile, decode, or otherwise attempt to derive or gain access to the source code, object code or underlying structure of the Service"*; §1.5(vi) prohibits *"probe, scan or attempt to penetrate the Service"*; §1.5(viii) prohibits *"harvest, scrape, or extract data from the Service"*. The reverse-engineered pattern (Path *not*-recommended, projects like `egoist/cursor-openai-api` and `wisdgod/cursor-api`) sits squarely inside these clauses. The CLI subprocess pattern is in a gray zone — the CLI is an official distribution channel and the subscription explicitly covers its use, but driving it programmatically from a third-party gateway has no explicit blessing in the terms.
- **Anthropic precedent** ([VentureBeat, 2026-01-09](https://venturebeat.com/technology/anthropic-cracks-down-on-unauthorized-claude-usage-by-third-party-harnesses); [mlq.ai, 2026-04-04](https://mlq.ai/news/anthropic-ends-paid-access-for-claude-in-third-party-tools-like-openclaw/)) — in April 2026 Anthropic cut Claude OAuth subscriptions off third-party harnesses (OpenClaw, OpenCode, Cursor) overnight. The same policy posture could land at Anysphere/Cursor at any time. This is the single biggest risk for the subscription-OAuth path and is the reason the *User API Key* path exists; we should treat the OAuth subscription path as best-effort, document the risk prominently, and have the User-API-Key / SDK path ready as a fallback.
- **Aliyun/Qwen ToS precedent** ([Aliyun](https://www.alibabalicloud.com/help/en/model-studio/token-plan-personal-overview), [Qwen Cloud](https://docs.qwencloud.com/token-plan/personal/token-plan-personal-overview)) — both restrict "Token Plan" keys to "interactive use within programming and agent tools (such as Claude Code, Cursor, ...)" and explicitly forbid *"API calls ... custom application backends, or any non-interactive batch call scenarios."* Cursor has not published equivalent language yet, but the precedent is real and worth flagging.
- **Recommendation:** surface a one-line warning in the provider README and a startup log message: *"This provider drives the user's Cursor CLI on their behalf. Use is governed by Cursor's Terms of Service. Use a Cursor User API Key (via the bridge) if you need a ToS-conservative path."*

### Maintenance / breakage

- The `wisdgod/cursor-api` archive note (*"Frequent upstream API breaking changes led to fragile workarounds"*) is the clearest empirical evidence that direct-backend reverse engineering breaks at least once per major Cursor release. The `7836246/cursor2api` "凉" note shows even the docs-page free endpoint is throttled out from under users.
- The CLI wrapper path is *also* at risk when Cursor changes the `agent` CLI's flags or `stream-json` schema — but the `agent --list-models` and `agent login` surface is much more stable than the internal protobuf, and any change to it would break Cursor's own users first (giving us advance signal).
- The official `cursor-sdk-bridge` is the lowest-risk path because Cursor commits to a versioning contract ([docs/versioning.md](https://github.com/cursor/sdk-bridge/blob/main/docs/versioning.md)) and the proto is regenerated on every SDK release.

### License compatibility

- MIT and Apache-2.0 (GoModel-friendly): `cursor/sdk-bridge`, `Sunnyender-org/cursor-sdk2api`, `tageecc/cursor-agent-api-proxy`, `egoist/cursor-openai-api`, `NGLSG/Cursor2API`, `7836246/cursor2api`, `mhrmsg/cursor-pro-account2api`, `router-for-me/CLIProxyAPI`.
- No license declared (`LICENSE` file absent): `anyrobert/cursor-api-proxy`, `Azhi-ss/cursorcli2api`. Borrow design patterns only; do not copy code verbatim without confirming the author intends MIT or writing a clean-room reimplementation.
- `NOASSERTION` (no granted reuse rights): `wisdgod/cursor-api`. Avoid wholesale copying; quoting + clean-room rewrite if any idea must be reused.

### Other engineering risks

- **CLI spawn cost / latency.** Each request forks a Node-or-Rust CLI subprocess (~200–500 ms cold). Mitigation: a small pool of warm subprocesses keyed by `(model, account)` (anyrobert's account-pool pattern; Azhi-ss's semaphore).
- **Streaming backpressure.** The CLI writes NDJSON to stdout; if the HTTP client is slow, the pipe buffer fills and the CLI blocks. Mitigation: drain stdout into a bounded channel, drop deltas with a "client slow" SSE comment if the channel is full (acceptable for chat).
- **Tool calls.** As noted, CLI mode is loss-loss on tool forwarding. If a downstream user needs tools, the choices are (a) accept loss-loss (drop them or surface as `reasoning_content`), or (b) switch to ACP mode at the cost of a more complex subprocess contract.
- **Process leaks.** A crashing client must still kill its child. Wire `cmd.Process.Kill()` into GoModel's request-context cancellation (Azhi-ss does this).

---

## Open questions deferred to other tickets

- **Token extraction shape** (`agent login` OAuth vs `CURSOR_API_KEY` vs browser cookie `WorkosCursorSessionToken`) — owned by #17.
- **Whether the Cursor "User API Key" from `cursor.com/settings` draws against the Pro/Ultra subscription quota or is metered separately** — owned by #17.
- **Grok model IDs visible on the subscription path** (does `agent --list-models` include Grok 4.x / Grok Code Fast at all Pro tiers?) — likely owned by a follow-up model-list ticket; `Sunnyender-org/cursor-sdk2api` README implies yes via the SDK (`Grok Build via /v1/responses`), but the CLI list needs empirical confirmation.
- **Lossy forwarding decision** (drop tool calls vs. implement ACP) — owner is the implementation planning ticket after #18 closes.

---

## Citations index

- cursor/sdk-bridge — <https://github.com/cursor/sdk-bridge> (MIT, Cursor-official)
- cursor/sdk-bridge docs/protocol.md — <https://github.com/cursor/sdk-bridge/blob/main/docs/protocol.md>
- cursor/sdk-bridge docs/streaming.md — <https://github.com/cursor/sdk-bridge/blob/main/docs/streaming.md>
- cursor/sdk-bridge docs/versioning.md — <https://github.com/cursor/sdk-bridge/blob/main/docs/versioning.md>
- @cursor/sdk on npm — <https://www.npmjs.com/package/@cursor/sdk>
- Cursor SDK changelog (2026-04-29) — <https://cursor.com/changelog/sdk-release>
- Cursor TypeScript SDK docs — <https://cursor.com/docs/sdk/typescript>
- Cursor Terms of Service — <https://cursor.com/terms-of-service>
- Cursor CLI installer — `curl https://cursor.com/install -fsS | bash` (referenced in every CLI-wrapper README above)
- Sunnyender-org/cursor-sdk2api — <https://github.com/Sunnyender-org/cursor-sdk2api>
- tageecc/cursor-agent-api-proxy — <https://github.com/tageecc/cursor-agent-api-proxy>
- tageecc/cursor-agent-api-proxy subprocess/manager.ts — <https://github.com/tageecc/cursor-agent-api-proxy/blob/main/src/subprocess/manager.ts>
- anyrobert/cursor-api-proxy — <https://github.com/anyrobert/cursor-api-proxy>
- anyrobert/cursor-api-proxy agent-runner.ts — <https://github.com/anyrobert/cursor-api-proxy/blob/main/src/lib/agent-runner.ts>
- anyrobert/cursor-api-proxy cli-stream-parser.ts — <https://github.com/anyrobert/cursor-api-proxy/blob/main/src/lib/cli-stream-parser.ts>
- egoist/cursor-openai-api — <https://github.com/egoist/cursor-openai-api>
- egoist/cursor-openai-api auth.ts — <https://github.com/egoist/cursor-openai-api/blob/main/src/auth.ts>
- egoist/cursor-openai-api proxy.ts — <https://github.com/egoist/cursor-openai-api/blob/main/src/proxy.ts>
- egoist/cursor-openai-api h2-bridge.mjs — <https://github.com/egoist/cursor-openai-api/blob/main/src/h2-bridge.mjs>
- egoist/cursor-openai-api models.ts — <https://github.com/egoist/cursor-openai-api/blob/main/src/models.ts>
- egoist/cursor-openai-api proto/agent_pb.ts — <https://github.com/egoist/cursor-openai-api/blob/main/src/proto/agent_pb.ts>
- Azhi-ss/cursorcli2api — <https://github.com/Azhi-ss/cursorcli2api>
- mhrmsg/cursor-pro-account2api — <https://github.com/mhrmsg/cursor-pro-account2api>
- NGLSG/Cursor2API — <https://github.com/NGLSG/Cursor2API>
- netandreus/pi-cursor-provider — <https://github.com/netandreus/pi-cursor-provider>
- fitchmultz/pi-cursor-sdk — <https://github.com/fitchmultz/pi-cursor-sdk>
- wisdgod/cursor-api (archived) — <https://github.com/wisdgod/cursor-api>
- 7836246/cursor2api — <https://github.com/7836246/cursor2api>
- 7836246/cursoride2api — <https://github.com/7836246/cursoride2api>
- router-for-me/CLIProxyAPI — <https://github.com/router-for-me/CLIProxyAPI>
- GitHub topic `cursor-api` — <https://github.com/topics/cursor-api>
- Anthropic crackdown on third-party harnesses (VentureBeat) — <https://venturebeat.com/technology/anthropic-cracks-down-on-unauthorized-claude-usage-by-third-party-harnesses>
- Anthropic ends paid access in OpenClaw (mlq.ai) — <https://mlq.ai/news/anthropic-ends-paid-access-for-claude-in-third-party-tools-like-openclaw/>
- Aliyun Token Plan overview — <https://www.alibabalicloud.com/help/en/model-studio/token-plan-personal-overview>
- Qwen Cloud Token Plan overview — <https://docs.qwencloud.com/token-plan/personal/token-plan-personal-overview>
- "How to Build a Harness Anthropic Won't Kill" (Hipolito) — <https://www.joeyhipolito.dev/log/2026-04-11-how-to-build-a-harness-anthropic-wont-kill>
- Bloom docs on Cursor Agent CLI — <https://docs.use-bloom.dev/agents/cursor>
- ADR precedent: gsd-build/gsd-2#6393 "Cursor as Model Provider via cursor-agent CLI / @cursor/sdk" — <https://github.com/gsd-build/gsd-2/issues/6393>
