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
`POST /v1/chat/completions` (stream and non-stream),
`POST /v1/web_search` (standalone retrieval, §2.3 / §7.1), `GET /v1/models`,
`GET /healthz`, optional bearer-key auth, OpenAI-shaped errors, a 60 s sliding
idle watchdog, and a disconnect-safe pool.

**Out of scope, deliberately:** usage/token accounting (upstream publishes
none; the field is omitted rather than fabricated), tool calling (no prompt
injection, no XML reassembler, no tool_choice), inline web search inside
`/api/chat` (`webSearchEnabled` is pinned off and `source-url` events are
ignored; the standalone endpoint is the supported search surface; the
`__searching__` / `__search_error__` sentinel titles are never reproduced),
the synthesized answer from `/api/search-lab/run` (only its sources are
bridged), admin UI, and persistence. CORS preflight is answered so browser
clients can call the keyed API.

The project is a single static binary with **zero non-stdlib dependencies**
(`go.mod` has no `require` block). See §1.1 for the pass-through philosophy
that constrains how much the gateway is allowed to validate.

## 1.1 Pass-through philosophy

The gateway is a **translator**, not a second product owner. The upstream is the
authority on what a Mercury request may contain: its limits, its parameter
semantics, and its error messages. Whenever the gateway rejects a request the
upstream could have answered, the gateway has invented a policy the user did not
ask for and broken clients that were previously innocent.

The gateway therefore rejects **only** these three classes:

1. **Unparseable transport payloads** — malformed JSON, trailing data after the
   request object, a body that is not a JSON object. There is nothing to
   translate, so there is nothing to forward.
2. **Structurally invalid requests** — a missing/empty `model`, missing/empty
   `messages`, a message with no renderable text at all, or a JSON value of the
   wrong type where the wire format requires one (e.g. `messages` not an array).
3. **Auth failures** — a missing or wrong API key when one is configured.

Everything else **flows upstream**, and the upstream's answer is translated
unchanged in message and content into the OpenAI error envelope.

Explicitly **not** validated (dropped, coerced, or passed through):

| Input | Behaviour | Rationale |
|---|---|---|
| `temperature`, `top_p`, `n`, `logprobs`, `top_logprobs`, `stop`, `presence_penalty`, `frequency_penalty`, `seed`, `user`, `response_format`, `stream_options`, `logit_bias`, `parallel_tool_calls`, any other unknown top-level key | **silently dropped** | The upstream has no equivalent field. OpenAI SDKs send these on every ordinary call; a 400 would break them over a field that changes nothing. |
| `tools`, `tool_choice`, `functions`, `function_call` | **dropped**; the request proceeds as plain chat | Tool calling is out of scope, but a caller that offers tools can still get an answer without them. |
| `messages[].tool_calls` / `function_call` | **dropped** | Assistant history produced by another tool-using agent is just text here. |
| `role:"tool"` / `role:"function"` | **coerced to `user`** | The tool result is text the model may find useful; erroring discards context the caller explicitly supplied. |
| Unknown roles | **coerced to `user`** | Same reasoning; the upstream would reject anything it truly cannot use. |
| `reasoning_effort` outside `low|medium|high` (incl. `minimal`) | **passed through** to the upstream after mapping `minimal→low` | The upstream owns its enum. |
| `content` parts other than `text` (`image_url`, `input_audio`, `file`, …) | text parts are kept; media parts are **rendered as a short text placeholder** so they are visible to the model, never a 400 | The upstream is text-only; the caller's intent survives as text. |
| Input length | **passed through**; no client-side 400 | The upstream truncates at its own context limit. That is the upstream's contract. |
| `/v1/web_search` unknown keys, out-of-range `fanout`/`depth`/`keep_top_k`, unknown `effort` | unknown keys **dropped**; numeric values **clamped** to the upstream's published ranges; `effort` **passed through** | The upstream answers what it can. |

What is **kept** because it is genuine gateway safety, not second-guessing
upstream policy:

- API-key auth (constant-time compare), CORS preflight, method routing.
- A generous request-body cap (`6 MiB`) that exists only to stop unbounded
  memory use, and is far above any request the upstream itself accepts.
- Request timeouts, the sliding idle watchdog, the disconnect-safe pool, the
  redirect policy that keeps `x-session-token` on-origin.
- Structural message checks: a message whose content yields **no** text at all
  is a 400, because the upstream wire format cannot represent it meaningfully.
- Upstream → OpenAI error mapping. A non-200 from `/api/chat` is surfaced as a
  502 with the upstream's message preserved (truncated for logs, but the client
  message names the upstream status); HTML challenge bodies are never echoed.

## 2. Upstream protocol (ground truth from the audit)

### 2.1 Session

```
GET https://chat.inceptionlabs.ai/api/session
-> 200 {"ok":true,"token":"<unix_ts>.<32 hex>.<64 hex>"}
```

Tokens are free, IP-unbound, and carry an embedded issue timestamp. They are
sent on chat requests as `x-session-token`.

Every request (session fetch, chat, search, and clearance acquisition) goes
through **one stdlib `http.Transport`** with `ForceAttemptHTTP2: true`, the
shared Chrome UA, and the `_vcrcs` clearance cookie when one is held. See §2.1.1
for the Vercel checkpoint bypass and §6 for the transport construction.

The gateway must **not** fail to boot when the pool cannot be filled; it serves
`503 upstream_unavailable` until a token can be acquired.

### 2.1.1 Vercel Security Checkpoint — `_vcrcs` clearance (proven design)

Ground truth: `/root/vercel-bypass/REPORT.md` and its working PoC
(`/root/vercel-bypass/poc/main.go`). The live edge answers
`429 + x-vercel-mitigated: challenge` unless **all** of the following hold:

| Condition | Notes |
|---|---|
| `_vcrcs` clearance cookie | `HttpOnly; Secure; SameSite=None; Max-Age=3600`; bound to (egress IP, UA, HTTP/2) |
| Chrome UA matching the one that solved | UA is part of the cookie binding |
| **HTTP/2** | h1.1 with a valid cookie is still answered `429`; this is protocol-level, not TLS-stack-level |

Not required (verified in 19 probes): `sec-ch-ua*`, `sec-fetch-*`, `referer`,
`accept-language`, header order, any other cookie, the telemetry body, a
warm-up GET of `/`, or any particular TLS fingerprint. Plain `crypto/tls` with
ALPN `h2` — i.e. a stock Go transport — is sufficient.

**The earlier design was wrong and is deleted.** The project used a `utls`
Chrome ClientHello with ALPN forced to `http/1.1` because the challenge was
believed to reject HTTP/2. The opposite is true: forcing h1.1 is precisely what
makes the edge re-issue a challenge on every request, so the gateway could
never hold a successful clearance. `utls`, `internal/fp`, the ALPN override,
and `DisableKeepAlives` are gone; `go.mod` has zero dependencies.

**Acquisition** (`internal/clear`), run lazily the first time a request needs
it, and again when the cookie is old:

```
GET  <base>/api/session                                  -> 429 + x-vercel-challenge-token
solve token locally (v2 PoW, below)                      ~70–100 ms CPU, no network
POST <base>/.well-known/vercel/security/request-challenge -> 204 + Set-Cookie: _vcrcs=… (3600 s)
```

The token format is
`<version>.<seed>.<ttl>.<base64(payload)>.<signature>`, where
`payload = <id>;<prefix>;<target>;<rounds>;<binary>`. For round `i`:

```
h_i      = 16 lowercase hex chars (any value; a counter nonce is fine)
digest_i = sha256(prefix || h_i) as 64 lowercase hex
require  digest_i starts with target[offset_i : offset_i+4]
next     target = digest_i[offset_{i+1} : offset_{i+1}+4]

offset_0   = (seed * M[seed mod 5]) mod 36
offset_i+1 = (seed * M[i mod 5])     mod 60
M = [498787, 533737, 619763, 708403, 828071]
```

The solution is `h_0;h_1;…;h_{n-1}` and is submitted with
`x-vercel-challenge-token`, `x-vercel-challenge-solution`,
`x-vercel-challenge-version: 2`. The solver is ported verbatim from the PoC
together with its captured-fixture tests (a real Chromium token/solution pair
must verify offline).

**Lifecycle.** One `Clearance` value per client, mutex-guarded, single-flight:

- `Header() (ua, cookie)` is consulted on every upstream request; empty cookie
  means "none held yet".
- `Refresh(ctx)` runs `GET → solve → POST` and stores the cookie with its
  issue time. It refuses to start another refresh within a 15 s cooldown per
  client, so a challenged burst cannot become a solve storm.
- The cookie is refreshed **proactively at 45 min of its 60 min TTL**.
- On a `429` carrying `x-vercel-mitigated: challenge` (or a challenge token
  header) the request is retried **once** after a forced refresh. A challenge
  that survives that replay is reported as `upstream_clearance_failed`, never
  handed back as an ordinary response body (a capability probe could otherwise
  misread it). A `403` challenge is surfaced, not retried: that means the PoW
  itself was rejected (version drift) and looping cannot fix it.
- Acquisition shares the client's `*http.Transport`, so it egresses through the
  configured proxy exactly like ordinary requests; the cookie is IP-bound, so
  this is mandatory, not an optimisation.

**Observability.** `/healthz` reports
`clearance: {has_cookie, age_seconds, refresh_count}`.

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
| `source-url` | `sourceId`, `url`, `title` | web search citation or sentinel (ignored) |
| `[DONE]` | — | normal end |

There are no `usage`, `finish-*`, `tool-*`, or `error` events. Multiple
reasoning and text blocks can interleave (web search produces
`reasoning-0 → text-0 → sources → reasoning-1 → text-1`). The diffusion model
often flushes the whole answer in one network read; TTFT can be 17 s on hard
prompts if reasoning is discarded, which is exactly why reasoning must be
forwarded.

Unknown event types and malformed `data:` lines are skipped and logged, never
fatal.

## 2.3 Standalone web search — `/api/search-lab/run`

The `/search` playground is backed by a standalone retrieval pipeline that is
callable without `/api/chat` (research: `docs/RESEARCH-websearch.md` §4).

### 2.3.1 Discovery

```
GET /api/search-lab/config
-> 200 {"modelLabel":"Mercury","retrievalAvailable":true,
        "defaults":{"fanout":4,"depth":2,"keepTopK":5,"effort":"instant"},
        "limits":{"minFanout":1,"maxFanout":5,"minDepth":1,"maxDepth":3}}
```

A response without a `retrievalAvailable` field (for example a checkpoint JSON
challenge) is **not** read as `false`: availability stays unknown. The probe
carries clearance and a pooled session token, because the live route is
challenged independently of `/api/session` and answers 401 to a cleared but
unauthenticated request. `retrievalAvailable:false` is the deployment
kill-switch. `limits` publishes
ranges for `fanout` and `depth` only; `keepTopK` has no documented range and
`effort` is verified only for `instant`.

### 2.3.2 Run

```
POST /api/search-lab/run
x-session-token: <token>            (recommended, not proven required)
Accept-Encoding: identity           (MUST — see below)
Content-Type: application/json

{"query":"...","config":{"fanout":3,"depth":1,"keepTopK":10,"effort":"instant"},
 "timezone":"UTC","language":"en-US"}
```

`effort: "instant"` is the only value the playground exercises and the only
one verified live; `fast` / `deep` are accepted pass-throughs.

Response: `application/x-ndjson; charset=utf-8`, one JSON object per line.
Observed event vocabulary (`t` discriminator; unknown fields ignored):

| `t` | Fields used | Meaning |
|---|---|---|
| `run_start` | `config` | run accepted |
| `call_start` | `kind`, `round`, `llm` | a pipeline stage begins (`plan`, `retrieve`, `rerank`, `followup`, `synthesize`) |
| `call_end` | `failed?`, `reason?` | stage ends (token counts are deliberately ignored) |
| `queries` | `queries: string[]` | plan / follow-up rewritten queries |
| `round_start` | `round`, `of`, `queries` | retrieval round begins |
| `candidates` | `count`, `fresh?` | documents retrieved before rerank |
| `docs` | `retrieved`, `kept`, `sources:[{title,url,score}]` | ranked, deduped sources |
| `rounds` | `run`, `of`, `stoppedEarly` | round accounting |
| `answer_first_token` | `ms` | synthesis TTFT |
| `answer_delta` | `d` | incremental synthesized answer |
| `run_done` | `ms`, `answer` | terminal success |
| `error` | `message` | terminal failure |

Terminal semantics: `run_done` or `error`. The body ending without either is a
truncation, never a success.

**Brotli trap.** The upstream returns `content-encoding: br` unless the client
advertises `identity`. The stdlib has no Brotli decoder and the project has no
third-party dependencies, so the client sends `Accept-Encoding: identity` and
decodes `gzip` (stdlib, defensive against intermediaries) normally. A `br`
body is a typed error naming the encoding rather than mojibake — this is the
one case where a documented upstream capability falls outside a zero-dependency
build; `identity` has been sufficient on every observed run.

**Snippets.** `docs.sources[]` carries only `title`, `url`, `score`; there is
no per-source snippet anywhere in the NDJSON — neither `docs` nor the
synthesis stage adds one. The gateway therefore returns `snippet: ""` and
does **not** wait for `run_done`. This is an honest empty field, not a
truncation. The SYNTHESIZE answer is discarded.

**Where the read stops.** Reading continues past the first `docs` when the
run has more rounds: with `depth > 1` the follow-up stage rewrites the query
set and a later round's `docs` is the final ranking. The gateway therefore
stops at the run's *final* `docs`, recognised by any of: the `rounds` event
with `run >= of`, a `call_start` with `kind:"synthesize"`, or `run_done`. It
never reads the synthesis deltas. A body that closes before any `docs` is
`upstream_stream_error`, never an empty success; a body that closes after one
is complete, because `docs` is the product.

**Titles** are raw upstream text with newlines and tab runs
(`"\n\tFour-day work week\n \n – …"`); they are whitespace-collapsed and
trimmed. Entries with a non-absolute `http`/`https` URL, or one carrying
control characters, are dropped — the documented consumer renders results as
links, so `javascript:`/`data:` must not reach it. `cite_index` is assigned
`1..N` in `docs` rank order, matching the bracket citations `[1][2][3]` in the
upstream's synthesized answer.

**Bounds.** Because the upstream is not trusted to be well-behaved, one run
retains at most 32 queries, 50 sources, 2000 runes per query, 512 per title,
and 4096 per URL. On top of the sliding watchdog, a whole run is capped at
ten idle windows (600 s with the default 60 s), so an upstream emitting one
keep-alive line per window cannot pin a request forever.

## 3. Wire format — OpenAI Chat Completions

### 3.1 Request

Decoded fields:

| Key | Handling |
|---|---|
| `model` | required; must be in the configured model list, else `404 model_not_found` |
| `messages` | required, non-empty; see §4 |
| `stream` | optional bool, default false |
| `reasoning_effort` | optional; `minimal` maps to `low`; anything else is passed through; default `medium` |

Every other top-level key is **silently dropped** (§1.1). That includes
`temperature`, `top_p`, `n`, `stop`, `logprobs`, `response_format`,
`presence_penalty`, `frequency_penalty`, `seed`, `user`, `stream_options`,
`tools`, `tool_choice`, `functions`, and `function_call`. A `tools` request is
answered as plain chat: tool calling is out of scope, so the tools are not
forwarded and no tool call is ever emitted.

The request body is capped at 6 MiB (`http.MaxBytesReader`). The cap exists
only to bound memory; it is far above any request the upstream accepts and is
not a substitute for the upstream's context limit. There is no client-side
length guard and no text-part count limit.

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
never `"length"` (there is no output cap). `source-url` events are ignored,
including the reserved `__searching__` / `__search_error__` sentinel titles
(the chat request pins `webSearchEnabled:false`, so they should never appear;
if the upstream emits them anyway, they are dropped like any other source).
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
| malformed JSON / trailing data / body not an object | 400 | `invalid_request_error` | `invalid_request_error` |
| missing model or messages, wrong JSON types, message with no text | 400 | `invalid_request_error` | `invalid_request_error` |
| unknown model | 404 | `invalid_request_error` | `model_not_found` |
| method not allowed | 405 | `invalid_request_error` | `method_not_allowed` |
| missing/bad API key | 401 | `invalid_request_error` | `invalid_api_key` |
| pool empty after acquisition attempts | 503 | `server_error` | `upstream_unavailable` |
| upstream non-200 | 502 | `upstream_error` | `upstream_error` |
| upstream idle timeout | 504 | `upstream_error` | `upstream_idle_timeout` |
| stream truncated / lost | 502 | `upstream_error` | `upstream_stream_error` |
| all auth retries rejected (401/403) | 502 | `upstream_error` | `upstream_auth_failed` |

Upstream non-200 responses are translated, never second-guessed: the client
receives `502 upstream_error` with a message that names the upstream status and,
when the upstream supplied one, its error text (sanitised, truncated at 4 KiB,
and never an HTML challenge page). Upstream HTML/challenge bodies are never
returned verbatim; they are logged server-side (truncated).

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

- `content` may be a string or an array of parts. Parts with
  `type:"text"` and a non-empty `text` become text parts. `image_url`,
  `input_audio`, `file`, and any other part type are **rendered as a short
  text placeholder** (`[image]`, `[audio]`, `[file]`, `[<type>]`) so the model
  sees that something was attached, and the request proceeds. Nothing is
  rejected for being media.
- A part that is not a JSON object, or a text part with a missing/empty
  string `text`, contributes no text. A message that ends up with **no**
  renderable text at all is the only content 400 (§1.1, class 2).
- `role:"tool"` and `role:"function"` messages are coerced to `user`;
  `tool_calls` / `function_call` on a message are dropped. Top-level `tools` /
  `tool_choice` are dropped and the request proceeds as plain chat.
- `name` is accepted and ignored.
- Unknown roles are coerced to `user`.
- There is no input length guard. The upstream truncates at its own limit;
  that is the upstream's contract (§1.1). The total text length is still
  counted and returned for logging.

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

- **One** `*http.Transport` per client process, constructed from stdlib only:
  `ForceAttemptHTTP2: true`, `TLSHandshakeTimeout`, `ResponseHeaderTimeout`,
  `Proxy` when configured, and `TLSClientConfig.RootCAs` for tests. Keep-alives
  are enabled: h2 multiplexing is the healthy path, and the clearance cookie
  lives on the connection's requests, not on a per-request handshake.
- Every request carries the shared Chrome UA constant and, once acquired, the
  `_vcrcs` cookie (§2.1.1). The UA is never caller-controlled, so acquisition
  and use can never diverge.
- `Open(ctx, req)` acquires a token and POSTs. On HTTP 401/403 it discards
  that token and retries with a new one, at most **2 retries** (3 attempts
  total). On a `429`/`403` challenge it refreshes clearance once and replays
  the request (§2.1.1). Other non-200 statuses return a typed error; the token
  is kept.
- No `http.Client.Timeout`: a whole-generation wall clock would kill long
  answers. Instead the stream reader owns a **sliding 60 s idle timer**,
  reset on every upstream line (including blank separator lines). Expiry
  cancels the request and returns `upstream_idle_timeout`.
- The same idle window bounds the whole **pre-response phase** (TCP connect,
  proxy tunnel, TLS handshake, and response headers). The dial and handshake
  are additionally bounded by `DialContext`/`TLSHandshakeTimeout`; the
  pre-header phase is cancelled from a timer so no phase is unbounded.
- Upstream redirects are refused when they leave the configured host or
  downgrade HTTPS, because `x-session-token` is a custom header that Go would
  otherwise copy to the redirect target.
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
| `/v1/web_search` | POST | yes | §7.1 |
| `/v1/models` | GET | yes | static list, `{object:"list",data:[{id,object:"model",created,owned_by:"inceptionlabs"}]}` |
| `/healthz` | GET/HEAD | no | `{"ok":true,"pool":{"size","min","max"},"search":{"available":bool|null},"counters":{...}}` |

### 7.1 `POST /v1/web_search`

Bridges §2.3 into the response contract used by Sliverkiss/simple-chat
(`app/internal/server/websearch_endpoint_test.go`), so a client written for
that gateway can switch base URL without changing its parser:

```json
// request
{"query": "latest tech news",
 "config": {"fanout": 1, "depth": 1, "keep_top_k": 3, "effort": "instant"}}
```

`config` is optional. `fanout` defaults to `3`, `depth` to `1`, `keep_top_k`
to `10`, `effort` to `instant`; `fanout`/`depth` are clamped to the upstream's
published 1–5 / 1–3 ranges and `keep_top_k` to 1–50 — clamping is a translation,
not a rejection. `effort` is passed through unchanged: the upstream owns its
enum. Unknown top-level and `config` keys are **silently dropped** (§1.1).
A missing or non-string `query` is a structural 400; an empty or
whitespace-only `query` is a 400. There is no query-length limit beyond the
body cap.

Response (200):

```json
{"object": "web_search",
 "queries": ["four-day workweek trial results countries"],
 "results": [{"url": "https://…", "title": "…", "snippet": "", "cite_index": 1}]}
```

- `queries` is every query from every `queries` event, in arrival order and
  deduplicated; empty arrays are `[]`, never `null`.
- `results` is the `docs` source list in rank order; `snippet` is always `""`
  (§2.3.2) and `cite_index` is `1..N`.
- No results is `200` with `"results":[]` — an honest empty list, not an
  error and not a retry.
- The endpoint is stateless and never streams; the whole NDJSON run is
  consumed under one request. The 200 response carries `Cache-Control:
  no-store` (results derive from the caller's query).

Error mapping (same envelope and codes as chat):

| Condition | HTTP | code |
|---|---|---|
| malformed JSON / missing or empty `query` / `query` not a string / `config` not an object / trailing JSON | 400 | `invalid_request_error` |
| method not allowed | 405 | `method_not_allowed` (`Allow: POST`) |
| pool empty | 503 | `upstream_unavailable` |
| `retrievalAvailable:false` (cached config probe) | 503 | `search_unavailable` |
| upstream non-200 | 502 | `upstream_error` |
| terminal NDJSON `error` event | 502 | `upstream_error` (upstream message logged, generic text returned) |
| NDJSON body ends without `docs`/`error` | 502 | `upstream_stream_error` |
| sliding 60 s idle watchdog (any line resets it) | 504 | `upstream_idle_timeout` |

`/healthz` gains `search.available`: `true` / `false` from the last
`/api/search-lab/config` probe, `null` before the first one completes or when
probing is disabled. The probe runs once at startup in a background goroutine
(bounded by `IL2API_SEARCH_PROBE_TIMEOUT`, default 15 s) and therefore never
blocks boot; a failed probe leaves the value `null` and does not disable the
endpoint. `IL2API_SEARCH_CONFIG_CHECK=off` skips the probe entirely.

Auth: when `IL2API_API_KEY` is non-empty, require
`Authorization: Bearer <key>` or `X-Api-Key: <key>`; comparison is
constant-time. An empty key disables auth.

Server hardening: `ReadHeaderTimeout` 10 s, request body cap 6 MiB, optional
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
| `IL2API_CONTEXT_CHARS` | `0` | deprecated no-op, retained for config compatibility; there is no input guard |
| `IL2API_CLEARANCE_REFRESH` | `45m` | proactive clearance refresh age (cookie TTL is 60m) |
| `IL2API_CLEARANCE_COOLDOWN` | `15s` | minimum spacing between clearance refreshes |
| `IL2API_SESSION_TIMEOUT` | `15s` | single `/api/session` fetch timeout |
| `IL2API_SEARCH_TIMEOUT` | `60s` | `/v1/web_search` sliding idle watchdog (the same window the chat path uses for `IL2API_IDLE_TIMEOUT`) |
| `IL2API_SEARCH_CONFIG_CHECK` | `on` | `on`/`off`; `off` skips the startup `/api/search-lab/config` probe (`search.available` stays `null`) |
| `IL2API_SEARCH_PROBE_TIMEOUT` | `15s` | bound on the one startup config probe |

Invalid values are startup errors with an actionable message.

## 9. Testing strategy

- Unit tests per package: config parsing, the PoW solver against the captured
  Chromium fixture, clearance lifecycle (acquire, cookie storage, proactive
  refresh, cooldown, 429 re-acquire, 403 surfaced) against a mock challenge
  endpoint, token shape, pool lifecycle with a fake fetcher and fake clock,
  request validation/drop behaviour, conversion, SSE event mapping, error
  envelopes, chunk shapes.
- The transport is exercised against a **local TLS listener** only (plus a
  local CONNECT proxy), never against Vercel. No live probing: the bypass is
  proven, and the only live check is the one final deployment smoke
  (`/healthz` + one chat + one search) described in the task.
- Integration tests run the real HTTP handlers against an `httptest` mock
  upstream, covering streaming framing, reasoning ordering, multi-block
  streams, snapshot text, mid-stream failure, idle timeout, auth, oversized
  input pass-through, dropped params, tools-as-plain-chat, media placeholder,
  and client disconnect.
- Web search is tested entirely against an `httptest` NDJSON mock: the full
  plan → retrieve → rerank → docs pipeline, a gzip-encoded body, a `br` body
  typed error, a missing `docs` event, a terminal `error` event, truncation,
  upstream non-200, `retrievalAvailable:false`, the sliding idle watchdog, and
  the startup config probe (success, failure, disabled).
- Target: ≥80 % statement coverage over `app/internal/...` and `app/main.go`
  helpers. Every commit passes `go build ./... && go vet ./... && go test ./...`.

## 10. Deployment

`app/` is the build root (Nixpacks scans it on belmo). The Dockerfile builds
with `golang:1.24-alpine` and runs as non-root on
`gcr.io/distroless/static-debian12:nonroot`, exposing port **3000**.
`docker-compose.yml` maps `3000:3000` and passes `IL2API_*` through.
`app/nixpacks.toml` pins `go build -o app .` / `./app`.

Because the upstream challenge is keyed to the egress IP and UA, the gateway
acquires its own clearance from whatever egress it actually uses; set
`IL2API_PROXY` to route both clearance acquisition and traffic through the same
proxy. If the edge still refuses (PoW version drift), `/healthz`' `clearance`
block shows `has_cookie:false` and the last refresh count, and chat serves
`503 upstream_unavailable` rather than looping.
