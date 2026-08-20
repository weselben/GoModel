# Cursor client surface as an upstream (cursor-cli / `cursor-agent`)

Research for weselben/GoModel#17 (wayfinder map: #16). Goal: decide how GoModel can use a
user's Cursor subscription as an upstream provider, mirroring the Claude Code pattern from
upstream PR ENTERPILOT/GoModel#706.

All endpoints and token internals below are **unofficial / reverse-engineered** unless the
citation is a `cursor.com/docs` page. Cursor does not publish a supported inference API for
subscription accounts; everything here can change without notice.

## 1. Authentication

### 1.1 The two official login methods

The Cursor CLI (`agent`, formerly `cursor-agent`) supports exactly two auth methods
([Authentication | Cursor Docs](https://cursor.com/docs/cli/reference/authentication)):

1. **Browser login** — `agent login` opens the browser against the user's Cursor account;
   `NO_OPEN_BROWSER=1 agent login` prints the URL instead (for SSH/headless boxes);
   `agent status` / `agent whoami` reports auth state; `agent logout` clears it.
2. **User API key** — a `crsr_...` key generated at Cursor Dashboard → API Keys, passed via
   `CURSOR_API_KEY` env var or `--api-key <key>`. This is the officially documented path for
   headless/CI use ([Using Headless CLI | Cursor Docs](https://cursor.com/docs/cli/headless)).

The browser flow itself is a WorkOS-backed session (`WorkosCursorSessionToken` cookie on
cursor.com), but that cookie only authenticates the **web dashboard API**, not the agent
backend ([CodexBar Cursor provider docs](https://github.com/steipete/CodexBar/blob/main/docs/cursor.md),
[cursor-pulse usage-API notes](https://github.com/cnwinds/cursor-pulse/blob/master/docs/cursor-usage-api.md)).

### 1.2 Where the credential is stored

CLI credential storage ([cursor-pulse](https://github.com/cnwinds/cursor-pulse/blob/master/docs/cursor-usage-api.md),
[Cursor forum: keychain fallback](https://forum.cursor.com/t/errsecitemnotfound-couldnt-find-your-saved-login-in-the-macos-keychain/167325)):

| Platform | Default | File fallback |
| --- | --- | --- |
| macOS | login Keychain, service `cursor-access-token` (`security find-generic-password -s "cursor-access-token" -w`) | `AGENT_CLI_CREDENTIAL_STORE=file` → `~/.cursor/auth.json` |
| Linux | `~/.config/cursor/auth.json` | same file |
| Windows | `%APPDATA%\Cursor\auth.json` | same file |

`auth.json` shape (written by `agent login` or by the API-key exchange at CLI startup):

```json
{ "accessToken": "eyJ...", "refreshToken": "eyJ...", "apiKey": "crsr_..." }
```

File permissions `0600`, directory `0700`
([cursor-pulse](https://github.com/cnwinds/cursor-pulse/blob/master/docs/cursor-usage-api.md)).
On macOS the Keychain default is a known pain point over SSH — tokens can't be written to the
Keychain from a remote session, which is why the file store env var exists
([forum: CLI login via SSH/Mosh](https://forum.cursor.com/t/cursor-cli-login-via-ssh-mosh/149045),
[forum: CLI broken via SSH](https://forum.cursor.com/t/cursor-cli-is-entirely-broken-via-ssh/141835)).

The **Cursor IDE** (not the CLI) stores its token separately in the VS Code-style state DB
`state.vscdb` under `ItemTable` keys `cursorAuth/accessToken` and `cursorAuth/refreshToken`
(Linux `~/.config/Cursor/User/globalStorage/state.vscdb`, macOS `~/Library/Application Support/Cursor/...`)
([eisbaw/cursor_api_demo](https://github.com/eisbaw/cursor_api_demo),
[CodexBar](https://github.com/steipete/CodexBar/blob/main/docs/cursor.md)).

### 1.3 Token shape, lifetime, refresh

- `accessToken` and `refreshToken` are JWTs; `accessToken` lifetime is **~1 hour**
  (`exp` ≈ 3601 s after issuance, measured)
  ([cursor-pulse](https://github.com/cnwinds/cursor-pulse/blob/master/docs/cursor-usage-api.md)).
- **API-key path (recommended for headless reuse):**
  `POST https://api2.cursor.sh/auth/exchange_user_api_key` with header
  `Authorization: Bearer crsr_...` and body `{}` returns
  `{ "accessToken": "eyJ...", "refreshToken": "eyJ..." }`. The `crsr_...` key is long-lived
  and can be re-exchanged repeatedly. The exchanged `refreshToken` can **not** be renewed via
  `POST /oauth/token` (server answers `shouldLogout: true`); on expiry you simply re-exchange
  the API key. Recommended client behavior: cache the access token in memory keyed by
  `sha256(api_key)`, refresh 5 min before JWT `exp`, re-exchange once on 401
  ([cursor-pulse](https://github.com/cnwinds/cursor-pulse/blob/master/docs/cursor-usage-api.md)).
- **IDE path:** IDE-issued refresh tokens *do* work with
  `POST https://api2.cursor.sh/oauth/token` (`grant_type=refresh_token`, `client_id`,
  `refresh_token`) ([eisbaw/cursor_api_demo](https://github.com/eisbaw/cursor_api_demo)).

### 1.4 Headless extraction verdict

Yes — the credential can be lifted and reused by another process:

- Simplest: read `~/.config/cursor/auth.json` (Linux) / `~/.cursor/auth.json` (macOS file
  mode) and use `accessToken` as a Bearer token until its JWT `exp`, then re-exchange
  `apiKey` (or the configured `CURSOR_API_KEY`) via `exchange_user_api_key`.
- macOS Keychain mode requires the `security` CLI and an unlocked login keychain — fine for
  a user-run gateway, awkward for daemons
  ([cursor-opencode-auth](https://github.com/R44VC0RP/cursor-opencode-auth) does exactly this).
- Cleanest operational story: GoModel accepts `CURSOR_API_KEY` (a `crsr_...` key) directly
  and does the exchange itself — no dependence on local CLI state at all.

## 2. Backend protocol

### 2.1 Hostnames

Reverse-engineered endpoint map ([eisbaw/cursor_api_demo](https://github.com/eisbaw/cursor_api_demo),
[cursor-opencode-auth](https://github.com/R44VC0RP/cursor-opencode-auth),
[cursor-pulse](https://github.com/cnwinds/cursor-pulse/blob/master/docs/cursor-usage-api.md)):

| Host | Role |
| --- | --- |
| `api2.cursor.sh` | Auth (`/auth/exchange_user_api_key`, `/oauth/token`), dashboard/usage RPCs |
| `api3.cursor.sh` | Telemetry |
| `agent.api5.cursor.sh` | Agent API, privacy mode |
| `agentn.api5.cursor.sh` | Agent API, non-privacy (what the CLI talks to) |
| `agentn.us.api5.cursor.sh` | Regional (US) variant selected by the CLI ([forum debug log](https://forum.cursor.com/t/cursor-cli-connection-lost-reconnecting/153691/40)) |
| `cursor.com/api/*` | Web dashboard (cookie auth only, Bearer rejected) |
| `api.cursor.com` | Official Team Admin API (Basic auth, admin keys only — not usable for inference) |

### 2.2 Transport and framing

Everything agent-facing is **Connect-RPC over HTTP/2**
([eisbaw/cursor_api_demo](https://github.com/eisbaw/cursor_api_demo),
[cursor-opencode-auth](https://github.com/R44VC0RP/cursor-opencode-auth)):

- Envelope: 5-byte frame header `[flags:1][length:4 BE]` + payload; flag bit 0 = gzip.
- Chat uses `Content-Type: application/connect+proto` (binary protobuf); some unary RPCs
  (model listing, dashboard) accept `application/json`.
- HTTP/2 is mandatory — the agent endpoint breaks on HTTP/1.1
  ([forum: worker receives httpVersion 1.1 but endpoint requires HTTP/2](https://forum.cursor.com/t/cursor-agent-cli-stuck-on-reconnecting-in-interactive-mode-worker-receives-httpversion-1-1-but-agent-endpoint-requires-http-2/159700)).

Required header profile (CLI flavor, per
[cursor-opencode-auth proxy-server.mjs](https://github.com/R44VC0RP/cursor-opencode-auth);
the IDE flavor adds `x-cursor-checksum`, `x-session-id`, OS/arch headers per
[eisbaw](https://github.com/eisbaw/cursor_api_demo)):

```
authorization: Bearer <accessToken>
content-type: application/connect+proto        # or application/json for unary RPCs
connect-protocol-version: 1
x-cursor-client-type: cli                      # "ide" for the IDE
x-cursor-client-version: cli-2026.01.09-231024f
x-ghost-mode: false
x-request-id: <uuid>
```

### 2.3 RPCs that matter

- **Chat (CLI agent run):** `POST https://agentn.api5.cursor.sh/agent.v1.AgentService/Run` —
  streamed protobuf request/response. The request message (`AgentClientMessage` →
  `AgentRunRequest`) carries a `ConversationAction` (user message + explicit context),
  `ModelDetails` (model id + display name), and a conversation id. The response is a stream
  of Connect frames containing protobuf `AgentServerMessage`s; assistant text arrives as
  nested string fields inside those frames. There is no public `.proto`; existing clients
  hand-roll the wire format or heuristically extract strings from the frames
  ([cursor-opencode-auth](https://github.com/R44VC0RP/cursor-opencode-auth)).
- **IDE chat:** `POST /aiserver.v1.ChatService/StreamUnifiedChatWithTools` on the api5 hosts —
  the IDE's richer streaming dialect (tool calls, checkpoints). More capable but more
  reverse-engineering surface ([eisbaw](https://github.com/eisbaw/cursor_api_demo)).
- **Model list:** `POST /agent.v1.AgentService/GetUsableModels` (JSON) →
  `{ "models": [{ "modelId": ... }] }` ([cursor-opencode-auth](https://github.com/R44VC0RP/cursor-opencode-auth));
  IDE equivalent `POST /aiserver.v1.AiService/AvailableModels`
  ([eisbaw](https://github.com/eisbaw/cursor_api_demo)). Both are Bearer-authenticated, so
  `/v1/models`-style discovery **is** implementable.
- **Usage/billing (for cost tracking):** Connect-RPC JSON
  `POST https://api2.cursor.sh/aiserver.v1.DashboardService/<Method>` with
  `Connect-Protocol-Version: 1`. Documented methods include `GetCurrentPeriodUsage`
  (plan spend in cents), `GetFilteredUsageEvents` (per-request events with
  `tokenUsage.inputTokens/outputTokens/cacheReadTokens/cacheWriteTokens/totalCents`,
  `model`, `kind`, `chargedCents`, `isHeadless`), `GetAggregatedUsageEvents`, `GetPlanInfo`,
  `GetHardLimit` ([cursor-pulse](https://github.com/cnwinds/cursor-pulse/blob/master/docs/cursor-usage-api.md)).
  **Important:** chat-stream frames do not carry clean usage stats; per-request token usage
  must be read back from `GetFilteredUsageEvents` (keyed by `conversationId` /
  `timestamp` / `model`).

### 2.4 Streaming shape

Not SSE. The agent stream is a sequence of Connect frames with protobuf payloads; the
assistant answer is embedded as length-delimited strings in later frames
([cursor-opencode-auth §extractTextFromResponse](https://github.com/R44VC0RP/cursor-opencode-auth)).
The only OSS client that does this today resorts to heuristic string extraction — a native
GoModel forwarding path would need the real `agent.v1` protobuf schema (extractable from the
CLI bundle; `eisbaw/cursor_api_demo` and the Go module `github.com/daotor/go-cursor`
([pkg.go.dev](https://pkg.go.dev/github.com/daotor/go-cursor), published Mar 2026, "verified
working — Connect protocol with manual protobuf wire-format encoding") are the two known
starting points).

## 3. Models under a Cursor subscription

### 3.1 Plan structure

Cursor plans have two usage pools ([Models & Pricing | Cursor Docs](https://cursor.com/docs/models-and-pricing)):

- **Cursor Models pool** (generous included usage): **Cursor Grok 4.6**, **Grok 4.5**, and
  **Composer 2.5**. These are first-party Cursor-hosted models and are exempt from the
  Cursor Token Rate. Grok 4.6 had a 50% launch-discount week starting 2026-08-12.
- **Other Models pool**: third-party models (OpenAI GPT-5.x, Anthropic Claude, Google
  Gemini, other xAI Grok variants) charged at list API price; Pro includes $20/mo, Pro+ $70,
  Ultra $400.

So the "Grok bundled with the subscription" the wayfinder cares about is precisely the
Cursor Models pool: Grok 4.6 / Grok 4.5. (Note: xAI's Grok is now labeled "SpaceXAI" in
Cursor docs; forum reports also mention Grok Code Fast as a third-party selection
([forum: model optimization guide](https://forum.cursor.com/t/how-to-optimize-your-usage-the-best-ai-models-to-use-version-3-0/145657)).)

### 3.2 Selection and discovery

- CLI: `--model <slug>` flag; `agent --list-models` / `agent models` lists available slugs
  for the account ([CLI reference](https://cursor.com/docs/cli/reference/parameters)).
  Observed slugs include `auto`, `gpt-5.2`, `gpt-5.3-codex`, `opus-4.6-thinking`,
  `sonnet-4.5-thinking`, `gemini-3-pro`
  ([cursor-agent-api-proxy README](https://github.com/tageecc/cursor-agent-api-proxy));
  Grok slugs follow the same pattern (e.g. `grok-4.5`
  ([forum: `--model grok-4.5`](https://forum.cursor.com/t/grok-model-selected-in-cli-usage-shows-gpt-5-6-sol-medium/165522))).
- Protocol: `GetUsableModels` (§2.3) returns the same list as `modelId`s — so model
  discovery does not require the CLI binary, only a valid access token.
- The model id travels in the `ModelDetails` message of `AgentRunRequest`
  ([cursor-opencode-auth](https://github.com/R44VC0RP/cursor-opencode-auth)).

## 4. Headless subprocess option (fallback integration shape)

`cursor-agent` is fully driveable non-interactively; this is the officially supported
automation path ([Headless CLI docs](https://cursor.com/docs/cli/headless),
[CLI parameters reference](https://cursor.com/docs/cli/reference/parameters)):

Canonical invocation (as used by existing wrappers,
[cursor_cli_sdk](https://github.com/nshkrdotcom/cursor_cli_sdk)):

```
agent -p --trust --output-format stream-json --stream-partial-output --model <slug> "prompt text"
```

Key flags: `-p/--print` (non-interactive), `--output-format text|json|stream-json`,
`--stream-partial-output` (token deltas in stream-json), `--model`, `--force/--yolo`
(apply edits), `--trust` (skip workspace trust prompt, headless only), `--resume [chatId]` /
`--continue` (session continuity), `--api-key` / `CURSOR_API_KEY`, `-H/--header`,
`--workspace <path>`, `--sandbox`, `--mode plan|ask`.

Output contract ([Output format docs](https://cursor.com/docs/cli/reference/output-format)):

- `text` (default): final answer text on stdout.
- `json`: one JSON object on completion, `{"result": "...", "chatId": "...", "model": "..."}`
  (per wrapper docs, e.g. [PraisonAI](https://praison.ai/docs/cli/cursor-cli)); deltas and
  tool events are not emitted.
- `stream-json`: NDJSON events — `{"type":"system","subtype":"init",...,"model":...}`,
  `{"type":"user",...}`, `{"type":"assistant","message":{"content":[{"type":"text","text":...}]}}`,
  `{"type":"tool_call","subtype":"started|completed","tool_call":{...}}`, and a terminal
  `{"type":"result","subtype":"success","duration_ms":...,"is_error":false,"result":"...","request_id":"..."}`.
  With `--stream-partial-output`, assistant events carry `timestamp_ms` deltas vs buffered
  flushes (distinguished by presence of `model_call_id`). Note the `result.result` field is
  the concatenation of assistant text, and no token-usage field is emitted.

Known operational caveats (all from Cursor forum bug reports):

- First connection can stall 10–15 s with zero output while the CLI retries across AWS
  backend IPs ([forum #150246](https://forum.cursor.com/t/cursor-agent-p-print-headless-mode-hangs-indefinitely-and-never-returns/150246)).
- Historical "hangs forever in -p mode" bugs (IPC deadlock between CLI and worker-server),
  claimed fixed in later CLI versions ([same thread](https://forum.cursor.com/t/cursor-agent-p-print-headless-mode-hangs-indefinitely-and-never-returns/150246)).
- Exit code is not fully trustworthy: a report shows exit 1 with empty stdout *after* a
  successful run when the chosen model name was later rejected
  ([forum #161075](https://forum.cursor.com/t/headless-agent-cli-exits-1-after-successful-gpt-5-5-run-stderr-cites-invalid-base-model-gpt-5-5/161075)).
  Wrappers must treat the stream-json `result` event as the source of truth, not the exit
  code, and impose their own overall timeout.
- The CLI refuses provider-BYOK keys — it only works with a Cursor account
  ([forum](https://forum.cursor.com/t/can-i-use-provider-api-keys-with-cursor-cli-agent/149158/4)),
  which is exactly the subscription path we want.

Existing subprocess-wrapper precedent: [tageecc/cursor-agent-api-proxy](https://github.com/tageecc/cursor-agent-api-proxy)
(spawns `agent` with stream-json, exposes `/v1/chat/completions` + `/v1/models` on :4646),
[anyrobert/cursor-api-proxy](https://github.com/anyrobert/cursor-api-proxy),
[netandreus/pi-cursor-provider](https://github.com/netandreus/pi-cursor-provider),
[nshkrdotcom/cursor_cli_sdk](https://github.com/nshkrdotcom/cursor_cli_sdk) (Elixir).

## 5. What GoModel should mirror

Recommendation, in preference order:

1. **Native upstream provider (primary), mirroring #706's shape but Connect-RPC instead of
   REST.** Credential: accept `CURSOR_API_KEY` (`crsr_...`) in provider config and/or detect
   `~/.config/cursor/auth.json` / `~/.cursor/auth.json` like #706 detects Claude OAuth
   tokens. Exchange via `POST api2.cursor.sh/auth/exchange_user_api_key`, cache the JWT in
   memory, refresh 5 min before `exp`, re-exchange on 401 (§1.3). Model discovery via
   `GetUsableModels` mapped into GoModel's `/v1/models`. Chat via
   `agent.v1.AgentService/Run` on `agentn.api5.cursor.sh` over HTTP/2 Connect-RPC, with
   protobuf request building and frame decoding — a `golang.org/x/net/http2` + hand-rolled
   protobuf path is small (the JS reference is ~150 lines) but needs the real message schema
   pinned from the CLI bundle, not heuristic string scraping.
2. **Usage tracking via dashboard RPCs, not stream parsing.** Since chat frames lack usage,
   mirror #706's "streaming usage observer" with a post-request lookup against
   `DashboardService/GetFilteredUsageEvents` (match on `conversationId`/timestamp/model) to
   populate GoModel usage records in cents + tokens (§2.3). Cost display should be a
   "subscription" mode: show Cursor-metered cents, mark as plan-included when
   `kind == USAGE_EVENT_KIND_INCLUDED_*`.
3. **Headless-subprocess provider as an opt-in fallback** (`cursor-cli` provider type that
   spawns `agent -p --output-format stream-json --stream-partial-output`). Zero
   reverse-engineering risk, officially supported auth, works the day the protobuf schema
   drifts. Costs: process spawn per request, no usage events in-band, exit-code unreliability
   (parse the terminal `result` event), first-connection latency spikes. This is also the
   fastest path to a working PoC and matches what the OSS ecosystem already ships.
4. **Do not** pursue the IDE dialect (`aiserver.v1.ChatService/StreamUnifiedChatWithTools`)
   or the web-cookie path; both add surface (checksum headers, machine-id binding, CSRF
   cookies) with no benefit over the CLI's own `agent.v1` API.

ToS risk stands as already noted in #16: subscription terms may restrict third-party gateway
use; document and let the user decide.

## 6. Open questions

- **Exact `agent.v1` protobuf schema** — field numbers for `AgentRunRequest` /
  `AgentServerMessage` beyond what `cursor-opencode-auth` hand-rolled (no tool calls, no
  thinking blocks). Needs extraction from the current CLI bundle or from `daotor/go-cursor`
  / `eisbaw/cursor_api_demo` sources; schema drift policy (pin `x-cursor-client-version`?)
  must be decided.
- **Token-usage timing** — how quickly `GetFilteredUsageEvents` reflects a just-finished
  agent run, and whether `conversationId` from our generated UUID is queryable (wrappers
  generate their own conversation ids, so likely yes, but unverified).
- **Multi-turn semantics** — `AgentRunRequest` carries a single `ConversationAction`;
  whether passing full history in `ExplicitContext` (the JS proxy's approach) is sufficient
  for GoModel's chat-completions workload, or whether server-side conversation state
  (`--resume`-equivalent) is required for correct billing/context.
- **Grok 4.6 slug confirmation** — exact `modelId` strings for the Cursor Models pool
  (`grok-4.6`? `cursor-grok-4.6`?) from a live `GetUsableModels` response; needs a real
  Pro account to verify (HITL).
- **Privacy vs non-privacy host** — whether token/plan determines `agent.api5.cursor.sh` vs
  `agentn.api5.cursor.sh` routing, and whether the wrong choice fails hard or silently
  downgrades.
- **Rate limits** on `exchange_user_api_key` and `AgentService/Run` are unpublished;
  backoff policy needed.
