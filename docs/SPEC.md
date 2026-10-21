# SPEC — inceptionlabs2api

Status: design of record. This document is written before the implementation;
the code and tests in `app/` implement it. Where this document and the audit
(`/tmp/audit-inception.md`) disagree, the audit is the upstream ground truth
and this document is the integration decision.

## 1. Purpose and non-goals

A minimal Go gateway that exposes the anonymous Inception Chat web endpoint
(`https://chat.inceptionlabs.ai`, Mercury 3 diffusion LLM) as an
OpenAI-compatible Chat Completions API.

**In scope:** anonymous session acquisition, a small token pool,
`POST /v1/chat/completions` (stream and non-stream), `GET /v1/models`,
`GET /healthz`, optional bearer-key auth, OpenAI-shaped errors, a 250k-character
input guard, a 60 s sliding idle watchdog, and a disconnect-safe pool.

**Out of scope, deliberately:** usage/token accounting (upstream publishes
none; the field is omitted rather than fabricated), tool calling (no prompt
injection, no XML reassembler, no tool_choice), web search (`source-url`
events are ignored), admin UI, and persistence. CORS preflight is answered so
browser clients can call the keyed API.

The project must remain a single static binary with one non-test external
dependency (`utls`).

## 2. Upstream protocol (ground truth from the audit)

### 2.1 Session

```
GET https://chat.inceptionlabs.ai/api/session
-> 200 {"ok":true,"token":"<unix_ts>.<32 hex>.<64 hex>"}
```

The transport is a `utls` Chrome client hello with ALPN forced to `http/1.1`
only: Vercel's bot challenge rejects the HTTP/2 JA4 on some networks. Tokens
are free, IP-unbound, and carry an embedded issue timestamp. They are sent on
chat requests as `x-session-token`.

The audit's P0 caveat applies: from some egress IPs `/api/session` answers
`429` with a Vercel HTML challenge even with the fingerprint transport. The
gateway must therefore **not** fail to boot when the pool cannot be filled; it
serves `503 upstream_unavailable` until a token can be acquired.

### 2.2 Chat

```
POST https://chat.inceptionlabs.ai/api/chat
x-session-token: <token>
Accept: text/event-stream, application/json
Content-Type: application/json
```

Request body (the fields the web client sends; all but `messages` are fixed):

```json
{
  "reasoningEffort": "medium",
  "webSearchEnabled": false,
  "voiceMode": false,
  "timezone": "UTC",
  "language": "en-US",
  "id": "c-<12 hex>",
  "messages": [
    {"parts": [{"type": "text", "text": "hi"}], "id": "m-<12 hex>", "role": "user"}
  ],
  "trigger": "submit-message"
}
```

Response: `text/event-stream` + `x-vercel-ai-ui-message-stream: v1`. Each
`data:` line is one JSON event; the stream ends with `data: [DONE]`.

Observed event vocabulary (the complete set):

| Event | Fields used | Meaning |
|---|---|---|
| `reasoning-start` | `id` | a reasoning block opens |
| `reasoning-delta` | `id`, `delta` | model-generated thinking summary |
| `reasoning-end` | `id` | block closes |
| `text-start` | `id`, optional `text` | a text block opens; `text` is a full snapshot |
| `text-delta` | `id`, `delta` | incremental answer text |
| `text-end` | `id` | block closes |
| `source-url` | `sourceId`, `url`, `title` | web search citation (ignored) |
| `[DONE]` | — | normal end |

There are no `usage`, `finish-*`, `tool-*`, or `error` events. Multiple
reasoning and text blocks can interleave (web search produces
`reasoning-0 → text-0 → sources → reasoning-1 → text-1`). The diffusion model
often flushes the whole answer in one network read; TTFT can be 17 s on hard
prompts if reasoning is discarded, which is exactly why reasoning must be
forwarded.

Unknown event types and malformed `data:` lines are skipped and logged, never
fatal.

## 3. Wire format — OpenAI Chat Completions

### 3.1 Request

Decoded fields (the only accepted keys):

| Key | Handling |
|---|---|
| `model` | required; must be in the configured model list, else `404 model_not_found` |
| `messages` | required, non-empty; see §4 |
| `stream` | optional bool, default false |
| `reasoning_effort` | optional `low` / `medium` / `high`, default `medium`; mapped to upstream `reasoningEffort` |

Every other top-level key is rejected with HTTP 400, `type:
invalid_request_error`, `code: unsupported_parameter`, and a message that
lists **all** offending keys plus the supported set. This is the explicit,
honest behaviour requested by the task (the audit found silent drops are
worse). `tools`, `tool_choice`, `functions`, and `function_call` get an
additional clause naming tool calling as unsupported.

The request body is capped at 2 MiB (`http.MaxBytesReader`); the 250k-character
guard means real requests stay far below it. Text parts are capped at 4096 per
request and empty text parts are rejected, so a part flood cannot bypass the
guard.

### 3.2 Response — streaming

Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
`X-Accel-Buffering: no`. HTTP status is committed only after the upstream
stream has been opened successfully.

Frames, in order:

1. role frame: `delta:{"role":"assistant"}` (immediately, before any upstream
   wait is observable)
2. zero or more `delta:{"reasoning_content":"..."}` frames, one per upstream
   `reasoning-delta`, in arrival order (including later blocks)
3. zero or more `delta:{"content":"..."}` frames, one per upstream
   `text-start` snapshot and `text-delta`
4. finish frame: `delta:{}`, `finish_reason:"stop"`
5. `data: [DONE]`

Every frame carries `id` (`chatcmpl-<12 hex>`, stable per request), `object:
"chat.completion.chunk"`, `created` (Unix seconds), `model`, and
`choices:[{index:0,delta:...,finish_reason:null|"stop",logprobs:null}]`.

`finish_reason` is `"stop"` when the upstream stream ends normally and is
never `"length"` (there is no output cap). `source-url` events are ignored.
`reasoning-start` / `reasoning-end` / `text-end` produce no frame of their
own.

If the upstream fails after the headers are committed, the gateway emits one
SSE frame `{"error":{"message","type":"upstream_error","code"}}` followed by
`data: [DONE]`, then returns. A stream never ends with a bare close.

### 3.3 Response — non-streaming

One JSON object:

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1790000000,
  "model": "mercury-3",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "...", "reasoning_content": "..."},
    "finish_reason": "stop",
    "logprobs": null
  }]
}
```

`reasoning_content` is omitted when empty. `content` is always a string
(`""` if the model produced only reasoning). There is **no** `usage` field —
the upstream publishes none and the gateway does not fabricate one. The
response is aggregated client-side from the upstream stream.

### 3.4 Errors

All non-streaming errors use OpenAI's envelope:

```json
{"error":{"message":"...","type":"invalid_request_error","code":"...","param":"..."}}
```

`param` is omitted when not applicable. Mapping:

| Condition | HTTP | type | code |
|---|---|---|---|
| malformed JSON / bad field value | 400 | `invalid_request_error` | `invalid_request_error` |
| unsupported parameter | 400 | `invalid_request_error` | `unsupported_parameter` |
| image/audio/file content part | 400 | `invalid_request_error` | `content_part_unsupported` |
| tool role / tool_calls / tools | 400 | `invalid_request_error` | `tool_calling_unsupported` |
| input > 250k characters | 400 | `invalid_request_error` | `context_length_exceeded` |
| unknown model | 404 | `invalid_request_error` | `model_not_found` |
| method not allowed | 405 | `invalid_request_error` | `method_not_allowed` |
| missing/bad API key | 401 | `invalid_request_error` | `invalid_api_key` |
| pool empty after acquisition attempts | 503 | `server_error` | `upstream_unavailable` |
| upstream non-200 | 502 | `upstream_error` | `upstream_error` |
| upstream idle timeout | 504 | `upstream_error` | `upstream_idle_timeout` |
| stream truncated / lost | 502 | `upstream_error` | `upstream_stream_error` |
| all auth retries rejected (401/403) | 502 | `upstream_error` | `upstream_auth_failed` |

Upstream HTML/challenge bodies are never returned to clients; they are logged
server-side (truncated).

## 4. Message conversion

For each OpenAI message, emit one upstream message with `parts`, a fresh
`m-<12 hex>` id, and the role:

| OpenAI role | Upstream role | Notes |
|---|---|---|
| `system` | `system` | text parts, no `state` |
| `developer` | `system` | alias, accepted and mapped |
| `user` | `user` | text parts |
| `assistant` | `assistant` | every text part gets `state:"done"` |

Rules:

- `content` may be a string or an array of `{type:"text",text}` parts. Only
  text parts are supported. `image_url`, `input_audio`, `file`, and any other
  part type produce the `content_part_unsupported` 400. An empty parts array
  is a 400.
- `role:"tool"`, a non-empty `tool_calls` on an assistant message, and
  top-level `tools` / `tool_choice` are rejected with
  `tool_calling_unsupported`; tool calling is out of scope.
- `name` is accepted and ignored.
- Unknown roles are a 400.
- The input guard counts Unicode code points over all text parts (not bytes,
  so CJK is counted honestly). More than 250,000 → `context_length_exceeded`
  before any upstream call. The limit is configurable
  (`IL2API_CONTEXT_CHARS`), and `-1` disables the guard.

## 5. Session pool

Token = `{unix_ts}.{32 hex}.{64 hex}`; shape is validated at acquisition.

- `min = 2`, `max = 8` (configurable), `ttl = 30m` (configurable).
- A background maintainer runs every 15 s: drop tokens older than TTL, then
  fetch until `size >= min`. Fetches are sequential inside the maintainer to
  avoid handshake storms.
- `Get(ctx)`: returns a live token round-robin. If the pool is empty it
  performs one synchronous fetch bounded by the caller context. No token is
  ever held exclusively (upstream tokens are stateless).
- `Discard(token)`: removes exactly that token and kicks one asynchronous
  refill. Only called on upstream HTTP 401/403.
- No error counting, no in-flight accounting, no `Release`, no persistence.
- Client disconnects, cancelled contexts, transport errors, timeouts, and
  upstream 5xx **never** discard or poison a token.
- Startup never fails because the upstream challenge blocks acquisition; the
  maintainer retries forever and the health endpoint reports `pool.size`.

## 6. Upstream client and stream reader

- One fresh fingerprint transport per chat request (`DisableKeepAlives`,
  ALPN `http/1.1`); this is required by the fingerprint trick and costs a
  TLS handshake per request — accepted and documented.
- `Open(ctx, req)` acquires a token and POSTs. On HTTP 401/403 it discards
  that token and retries with a new one, at most **2 retries** (3 attempts
  total). Other non-200 statuses return a typed error; the token is kept.
- No `http.Client.Timeout`: a whole-generation wall clock would kill long
  answers. Instead the stream reader owns a **sliding 60 s idle timer**,
  reset on every upstream line (including blank separator lines). Expiry
  cancels the request and returns `upstream_idle_timeout`.
- `[DONE]` → `io.EOF`. A clean EOF before `[DONE]` → `upstream_stream_error`
  (never silently successful).
- A single malformed JSON line or unknown event type is skipped, not fatal.
- Lines are read with a 16 MiB scanner buffer.
- Client cancellation propagates through the request context and closes the
  body; it is not an upstream error and does not touch the pool.

## 7. HTTP server

| Route | Method | Auth | Behaviour |
|---|---|---|---|
| `/v1/chat/completions` | POST | yes | §3 |
| `/v1/models` | GET | yes | static list, `{object:"list",data:[{id,object:"model",created,owned_by:"inceptionlabs"}]}` |
| `/healthz` | GET/HEAD | no | `{"ok":true,"pool":{"size","min","max"},"counters":{...}}` |

Auth: when `IL2API_API_KEY` is non-empty, require
`Authorization: Bearer <key>` or `X-Api-Key: <key>`; comparison is
constant-time. An empty key disables auth.

Server hardening: `ReadHeaderTimeout` 10 s, request body cap 2 MiB, optional
proxy credentials redacted from logs, upstream redirects restricted to the
configured origin (no cross-host or HTTPS→HTTP hops, since the session token
rides a custom header). CORS runs outside auth so preflights are answerable.

## 8. Configuration (env only, `IL2API_*`)

| Env | Default | Meaning |
|---|---|---|
| `IL2API_ADDR` | `:3000` | listen address |
| `IL2API_API_KEY` | empty | bearer key; empty = auth off |
| `IL2API_UPSTREAM_BASE` | `https://chat.inceptionlabs.ai` | upstream origin |
| `IL2API_MODEL` | `mercury-3` | default/primary model id |
| `IL2API_MODELS` | value of `IL2API_MODEL` | comma-separated accepted ids |
| `IL2API_PROXY` | `$HTTPS_PROXY` → `$HTTP_PROXY` → `$ALL_PROXY` | outbound HTTP CONNECT proxy (`http://host:port`, `host:port`, or `https://`); optional `user:pass@` is sent as Proxy-Authorization and redacted from logs |
| `IL2API_POOL_MIN` | `2` | minimum live tokens |
| `IL2API_POOL_MAX` | `8` | maximum live tokens |
| `IL2API_POOL_TTL` | `30m` | token lifetime |
| `IL2API_IDLE_TIMEOUT` | `60s` | stream idle watchdog |
| `IL2API_CONTEXT_CHARS` | `250000` | input character guard; `-1` disables |
| `IL2API_SESSION_TIMEOUT` | `15s` | single `/api/session` fetch timeout |

Invalid values are startup errors with an actionable message.

## 9. Testing strategy

- Unit tests per package: config parsing, fingerprint ALPN spec, token shape,
  pool lifecycle with a fake fetcher and fake clock, request validation and
  conversion, SSE event mapping, error envelopes, chunk shapes.
- The utls dialer is exercised against a **local TLS listener** only (plus a
  local CONNECT proxy), never against Vercel.
- Integration tests run the real HTTP handlers against an `httptest` mock
  upstream, covering streaming framing, reasoning ordering, multi-block
  streams, snapshot text, mid-stream failure, idle timeout, auth, the context
  guard, and client disconnect.
- Target: ≥80 % statement coverage over `app/internal/...` and `app/main.go`
  helpers. Every commit passes `go build ./... && go vet ./... && go test ./...`.

## 10. Deployment

`app/` is the build root (Nixpacks scans it on belmo). The Dockerfile builds
with `golang:1.24-alpine` and runs as non-root on
`gcr.io/distroless/static-debian12:nonroot`, exposing port **3000**.
`docker-compose.yml` maps `3000:3000` and passes `IL2API_*` through.
`app/nixpacks.toml` pins `go build -o app .` / `./app`.

Because the upstream challenge is IP-sensitive, a deployment whose egress is
challenged will start and serve `503 upstream_unavailable`; set
`IL2API_PROXY` to route through a non-challenged egress.
