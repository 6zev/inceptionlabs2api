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
`POST /v1/web_search` (standalone retrieval, §2.3 / §7.1),
`POST /v1/audio/speech` (TTS, §2.5.1 / §7.2),
`POST /v1/audio/transcriptions` (STT, §2.5.2 / §7.3), `GET /v1/models`,
`GET /healthz`, optional bearer-key auth, OpenAI-shaped errors, a 60 s sliding
idle watchdog, and a disconnect-safe pool.

**Out of scope, deliberately:** usage/token accounting (upstream publishes
none; the field is omitted rather than fabricated), tool calling (no prompt
injection, no XML reassembler, no tool_choice), the synthesized answer from
`/api/search-lab/run` (only its sources are bridged), admin UI, and
persistence. Inline web search inside `/api/chat` is **on** — it is the
upstream default for a new chat and its `source-url` events are translated
into OpenAI `url_citation` annotations (§2.4). CORS preflight is answered so
browser clients can call the keyed API.

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
| `/v1/web_search` unknown keys, out-of-range `fanout`/`depth`, unknown `effort` | unknown keys **dropped**; `fanout`/`depth` **clamped** to the ranges the upstream publishes in `/api/search-lab/config`; `keep_top_k` and `effort` **passed through**; no count caps on queries or sources | Clamping two values the upstream documents is translation; inventing a range for `keep_top_k` (none is published) would be second-guessing. Counts are bounded by the NDJSON transport (§2.3.2). |
| `/v1/audio/speech` `voice` | OpenAI voice names **mapped** onto upstream voices; any other name **passed through unchanged** | The upstream accepts every string and answers with its own behaviour (a 0.44 s clip for an unknown name); rejecting would be an invented policy (§2.5.1). |
| `/v1/audio/speech` `response_format`, `speed`, unknown keys | `response_format` accepted and ignored (mp3 is the only format the upstream produces); `speed` **dropped silently**; unknown keys **dropped** | There is no upstream knob. The client asked for a format the upstream cannot make; the gateway serves what exists (§2.5.1). |
| `/v1/audio/speech` `input` length | **passed through**; no client-side cap | The playground truncates at 5 000 chars client-side; the upstream's own behaviour above that is undefined, so the gateway lets it answer. |
| `/v1/audio/transcriptions` `language`, `prompt`, `temperature`, unknown fields | **dropped silently** | The upstream accepts only `file`. |
| `/v1/audio/transcriptions` non-MPEG audio | **passed through unchanged**, part typed by sniffing | The upstream's contract is MP3-only; the gateway does not transcode (no ffmpeg, §2.5.2). |

What is **kept** because it is genuine gateway safety, not second-guessing
upstream policy:

- API-key auth (constant-time compare), CORS preflight, method routing.
- A generous request-body cap (`6 MiB`) that exists only to stop unbounded
  memory use, and is far above any request the upstream itself accepts.
- Request timeouts, the sliding idle watchdog, the disconnect-safe pool, the
  redirect policy that keeps `x-session-token` on-origin.
- Structural message checks: a message whose content yields **no** text at all
  is a 400, because the upstream wire format cannot represent it meaningfully.
- `model` validation on `/v1/chat/completions` (§3.1, decision below).
- URL filtering on the two source lists (`/v1/web_search` `results`,
  `url_citation` annotations): only absolute `http(s)` URLs with a host and no
  control characters are emitted (§2.3.2, §2.4). This is not a judgement about
  the upstream's answer; it protects the gateway's own documented response
  shape, which clients render as links.
- Upstream → OpenAI error mapping. A non-200 from `/api/chat` is surfaced as a
  502 with the upstream's message preserved (truncated for logs, but the client
  message names the upstream status); HTML challenge bodies are never echoed.

**Model names are the one protocol-level rejection.** `/v1/models` is a static
list the gateway publishes; OpenAI clients use that list (and a 404
`model_not_found`) to decide whether a model id exists. There is exactly one
upstream engine and the `/api/chat` wire format has no model field, so an
unknown id cannot be "forwarded and judged upstream" — there is nothing to
forward it to. The gateway therefore (a) answers `404 model_not_found` for an
id outside `IL2API_MODELS`, and (b) does **not** silently rewrite an unknown id
to `mercury-3`: a client asking for a different model must be told it does not
exist, not served Mercury under someone else's name. This rejection is part of
the gateway's own OpenAI surface, not an invented upstream policy.

**The upstream is not trusted for shape, only for policy.** Two classes of
client-side work therefore remain even under this philosophy, and are not
rejections:

1. *Sanitising our own output shape* — dropping non-http URLs, collapsing
   whitespace in titles, capping a title's length. The gateway's documented
   contract says `results[].url` is a link and `annotations[]` are
   `url_citation`s; a `javascript:` URL would make that contract false (and be
   executable in the consumer's origin). This is applied to what the upstream
   sends, never to what the client sends.
2. *Bounding what a hostile upstream can make us retain* — response-body caps
   and the absolute search deadline. These are memory/time safety on the
   gateway's own resources, not an opinion about the request.

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

**Acquisition** (`internal/clear`), started at boot and again when the cookie
is old:

```
GET  <base>/api/session                                  -> 429 + x-vercel-challenge-token
solve token locally (v2 PoW, below)                      ~70–100 ms CPU, no network
POST <base>/.well-known/vercel/security/request-challenge -> 204 + Set-Cookie: _vcrcs=… (3600 s)
```

**Prewarm semantics.** Boot starts clearance acquisition immediately in a
background goroutine (`Client.StartClearancePrewarm`): it never blocks or
fails boot, and a failure is logged and left to the ordinary retry paths
(lazy `Ensure` on the next request, `Refresh` on a challenge). Because the
solve is ~100 ms and the acquisition shares the gateway's egress, `/healthz`
reports `clearance.has_cookie:true` within about a second of boot in the
normal case. A background refresher (`RefreshLoop`) then re-acquires at the
configured `IL2API_CLEARANCE_REFRESH` age (45 m of the 60 m TTL), measured
from boot rather than from the first request.

### 2.1.2 Clearance ring — design and the evidence behind it

A single cookie is a single point of revocation: one challenge on one request
forces a re-solve that every in-flight request then waits on. The gateway
therefore holds a small **ring** of clearance cookies in the one `Clearance`
value (§2.1.1), sized by `IL2API_CLEARANCE_POOL` (default 3, `1` restores the
old single-cookie behaviour).

The ring is justified by one question the SPEC could not answer from the
audit: **are clearance cookies interchangeable?** Six live probes on
2026-10-21 (≤6 total, ~15 s apart, the operator's probe budget) answered it:

| # | Probe | Result |
|---|---|---|
| 1 | Acquire three cookies from the same egress (`GET /api/session` → solve → `POST request-challenge`, three times) | Three **distinct** cookie values. The timestamp and signature parts differ; the base64 id part is identical. Each acquisition is a separate solve. |
| 2 | `GET /api/session` with cookie A only | `200` |
| 3 | `GET /api/session` with cookie B only | `200` — **cookies are fungible**; no acquisition is "the one" |
| 4 | Cookie A 10× back-to-back (sub-second), then cookie B, then A again | `200` on all 12; no per-cookie penalty, no edge rate score that retires a hot cookie |
| 5 | `POST /api/tts` with `voice:"garbage-voice"` | `200`, 12 582 bytes, `ID3` header — the upstream front door is the same one the gateway uses, and it accepts arbitrary voices |
| 6 | `POST /api/stt` with a 2.5 MB MP3 built by repeating frames | `200` after **30.9 s** with a (repeated) transcript — the upstream accepts a 2.5 MB body and does not 413 it; it does, however, take ~12 s/MB |

Conclusions the ring is built on:

- **Fungible, no per-cookie penalty.** Probe 4 is the evidence: ten rapid
  requests on one cookie did not retire it and did not penalise cookie B.
  A ring of 3 therefore buys (a) a request that gets challenged retires only
  one slot, and (b) rotation spreads per-cookie request counts, which is a
  cheap hedge against a penalty that may exist at volumes the probes cannot
  reach. It does **not** buy parallelism for its own sake.
- All ring slots must come from the **same egress** (the cookie is IP-bound,
  §2.1.1). Acquisition shares the transport, so this is automatic.
- Cookies are acquired **sequentially**, not concurrently: the challenge is
  per-request and a concurrent burst is exactly the solve storm the cooldown
  exists to prevent. Boot fills the ring behind the non-blocking prewarm.

**Ring lifecycle.** `IL2API_CLEARANCE_POOL` slots, each `{cookie, acquired}`:

- `Header()` (called once per upstream request) returns the next slot
  **round-robin, rotating once per request** — not once per token or per
  retry — together with the shared UA. A slot past `IL2API_CLEARANCE_REFRESH`
  is skipped if another live slot exists; a request never blocks on the ring.
- `Ensure(ctx)` acquires only when **no** slot is live. The boot prewarm fills
  empty slots to the configured size; a partially filled ring serves
  immediately and fills the rest lazily.
- The background `RefreshLoop` checks every slot and replaces the ones at the
  refresh margin, one per tick, so a 3-slot ring re-solves three times across
  three ticks rather than in one burst.
- **429 on a slot** retires *that slot only*: the request replays once with a
  freshly acquired cookie (the play is made with a second, still-live slot if
  one exists), and the retired slot is refilled in the background. Other
  slots are untouched, and their ages are unaffected.
- A probe of an unchallenged egress (no cookie) is recorded once
  (§2.1.1, L3); the ring does not manufacture cookies on an egress that does
  not need them.
- The failed-acquisition cooldown is ring-wide (one solve storm guard), while
  retirement and refill are per-slot.

**Observability.** `/healthz` reports
`clearance: {has_cookie, age_seconds, refresh_count, slots:[{cookie:bool,
age_seconds, refresh_count}]}`. The top-level fields describe the oldest live
slot (`has_cookie` false when the ring is empty); `slots` has one entry per
configured slot so an operator can see a single retired slot against a healthy
ring. `refresh_count` counts acquisitions for that slot.

**Why not a bigger pool.** Each acquisition is a real solve (~50–200 ms CPU
observed in the probes) plus a POST; the edge challenge is per-request and
IP-scoped, so more than a handful of slots adds solve traffic without adding
protection the evidence supports. 3 is the default; the knob exists for
operators who observe a per-cookie limit the probes did not.

**Cold boot.** Boot order is: transport → clearance prewarm (background) →
pool maintainer starts filling `IL2API_POOL_MIN` (which needs clearance, so it
waits for the first solve) → search config probe (needs a pooled token, so it
waits for the pool). Fully-warm state — ring at size, pool at min, search
probed — is reached in about one solve (~0.1–0.4 s) plus one session fetch,
measured under 1 s locally; the audio probe (30.9 s for 2.5 MB) shows the
upstream can be slow, but none of those phases blocks boot. `/healthz` is
reachable from the first second and reports each phase as it lands.

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

**Lifecycle.** One `Clearance` value per client (a ring of cookies, §2.1.2),
mutex-guarded, single-flight:

- `Header() (ua, cookie)` is consulted on every upstream request and rotates
  the ring; empty cookie means "no slot is live yet".
- `Refresh(ctx)` runs `GET → solve → POST` and stores a cookie in a slot with
  its issue time. A **failed** acquisition opens a 15 s cooldown per client,
  so a challenged burst — or a checkpoint that keeps refusing — cannot become
  a solve storm. Inside the window a live cookie is reused, and a caller
  without one receives the recorded failure rather than a silent success. A
  successful solve never opens a window, so the replay after a 429 is never
  blocked.
- An egress the edge does not challenge is recorded as such (with its
  timestamp) instead of being probed before every request; it is re-probed on
  the same schedule a cookie would be refreshed.
- Each cookie is refreshed **proactively at 45 min of its 60 min TTL**, and
  that cycle starts at boot (§ acquisition above), not at first traffic.
- On a `429` carrying `x-vercel-mitigated: challenge` (or a challenge token
  header) that ring slot is retired, the request is retried **once** with a
  fresh cookie, and the retired slot is refilled in the background. A
  challenge that survives that replay is reported as
  `upstream_clearance_failed`, never handed back as an ordinary response body
  (a capability probe could otherwise misread it). A `403` challenge is
  surfaced, not retried: that means the PoW itself was rejected (version
  drift) and looping cannot fix it.
- Acquisition shares the client's `*http.Transport`, so it egresses through the
  configured proxy exactly like ordinary requests; the cookie is IP-bound, so
  this is mandatory, not an optimisation.

**Observability.** `/healthz` reports the ring (§2.1.2).

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
| `source-url` | `sourceId`, `url`, `title` | web search citation or sentinel (§2.4) |
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

**Bounds.** The only numeric bounds on the *client's* request are `fanout` and
`depth`, clamped to the values the upstream publishes (§7.1). There is no
client-side `keep_top_k` range: the upstream publishes none, so the value is
passed through, and a number that is out of range is the upstream's answer to
give. There is no client-side query-length limit beyond the body cap.

On the *response* side the upstream is not trusted for shape or liveness, and
three counts are bounded for that reason, not policy reasons:

- Retained queries: at most 32 per run (a real run's maximum is
  `fanout × rounds`; 32 is far above it) — keeps a hostile stream from growing
  the response without limit.
- Retained sources: at most 50 per run, in rank order — keeps both the
  `/v1/web_search` response and an annotation array bounded.
- 2000 runes per query, 512 per title, 4096 per URL — the URL and title
  bounds feed the same `isHTTPURL` / whitespace rules as §2.4.

A whole run is additionally capped at ten idle windows (600 s with the default
60 s), so an upstream emitting one keep-alive line per window cannot pin a
request forever.

### 2.4 Search citations — translating `source-url`

The upstream answer text carries bracket markers (`[13]`, `[19]`) that refer to
the sources it retrieved. The gateway must not let those markers arrive with no
sources attached, so `source-url` events become OpenAI `url_citation`
annotations.

**Captured event shape** (one authorized live probe, 2026-10-21; raw stream in
`docs/source-url-capture.sse`):

```
data: {"type":"source-url","sourceId":"__searching__","url":"","title":"__searching__"}
data: {"type":"source-url","sourceId":"https://autonomy.work/portfolio/uk4dwpilotresults","url":"https://autonomy.work/portfolio/uk4dwpilotresults","title":"The results are in: the UK's four-day week pilot - The Autonomy Institute"}
…
```

Fields, and nothing else: `type`, `sourceId`, `url`, `title`. There is **no
per-source index, rank, or offset field** in the event.

**Rank mapping.** `sourceId` equals the URL for a real source, so it carries no
ordering either. The only ordering the stream supplies is **arrival order**,
which is also how the model numbers citations in practice. The gateway assigns
`index = arrival rank` over the accepted sources, starting at 1. This mapping
is an assumption, documented here because the upstream does not publish one.

**Sentinels.** `title == "__searching__"` opens the search phase: a no-op.
`title == "__search_error__"`: log it and keep the answer flowing; a search
failure is never allowed to abort the chat stream. Both sentinels are dropped
and never become annotations, and `__searching__` does not consume a rank.

**Acceptance rules** (each accepted source is one annotation):

- `url` must be an absolute `http`/`https` URL with a host, carry no ASCII
  control characters, and be at most 4096 runes; otherwise it is dropped
  (same `isHTTPURL` rule as `/v1/web_search`, so `javascript:`/`data:` never
  reach a client that renders links).
- `title` is whitespace-collapsed and trimmed, capped at 512 runes.
- Duplicate URLs are dropped (the first occurrence keeps its rank).
- At most 50 annotations per response.

**Annotation shape** (identical in both paths):

```json
{"type":"url_citation","index":1,"url":"https://…","title":"…","start_index":0,"end_index":0}
```

`start_index`/`end_index` are **always 0**: the upstream events carry no
character offsets into the answer text, and 0/0 is OpenAI's convention for
"not available". The gateway does not invent offsets by searching the answer
for a bracket marker, because a marker may repeat and the assumption would be
silent and wrong. (OpenAI's field names are `start_index`/`end_index`.)

**Where annotations attach.**

- **Streaming:** after the upstream text blocks that precede the sources have
  ended — concretely, on the gateway's `finish_reason:"stop"` frame, whose
  `delta` is `{"annotations":[…]}` instead of `{}` — one chunk carrying the
  whole array. This is the "annotations on the finish chunk" alternative from
  the task, chosen because the upstream interleaves `text-end → sources →
  text-start` and any earlier chunk could be followed by more text. A stream
  with no accepted sources keeps the plain `delta:{}` finish frame.
- **Non-streaming:** `choices[0].message.annotations` holds the same array in
  response order. The field is omitted when there are no sources.

Sources are collected in arrival order and emitted even when the upstream
places them after the first text block and a later block follows (§2.4 probe).

### 2.5 Audio — `/api/tts` and `/api/stt`

Both routes live behind the same transport as chat: clearance cookie,
`x-session-token`, HTTP/2. Neither is streamed by the upstream in any
meaningful sense — TTS returns one complete body, STT returns one JSON object.

#### 2.5.1 TTS — `POST /api/tts`

```
POST /api/tts
content-type: application/json
x-session-token: <token>

{"text":"Capability probe audio test one.","voice":"sarah"}
```

`voice` is optional. Response: `200 audio/mpeg` — a complete MP3 file
(ID3v2.4 header, MPEG-1 Layer III, 128 kbps, 44.1 kHz, mono). No `model`,
`format`, `speed`, or `response_format` field exists; anything beyond `text`
and `voice` is ignored.

**Voice names are not validated upstream.** An unknown name answers `200` with
a short clip (~0.44 s for the names the audit probed; probe 5 of §2.1.2 got
12 582 bytes for `garbage-voice`) — an odd-but-valid answer, not an error.
Under §1.1 that answer is the upstream's to give, so the gateway does **not**
reject an unknown voice: it forwards it and the client receives whatever the
upstream produces. This is the honest option; a 400 would be an invented
rejection of a request the upstream readily answers.

**Voice mapping (bundle archaeology + 3 confirmation probes, 2026-10-21).**
The bundle never enumerates voices: no picker array, no voice-id list. The
only voice names present anywhere in the 20 chunks are the scenario defaults
`voice:"sarah"` and the four names in the `/api/voice/config` scenarios.
The live config (captured `outF/voice-config-full`) names exactly **four**:
`jessica`, `chris`, `charlie`, `sarah`. Three confirmation probes were run
with identical text (`v5-voice-*`, 2026-10-21) and every name produced a
distinct file (MPEG audio hashes differ):

| Voice | Bytes | Audio | Duration |
|---|---|---|---|
| `chris` | 33 480 | 33 436 | 2.090 s |
| `charlie` | 35 570 | 35 526 | 2.220 s |
| `sarah` | 36 406 | 36 362 | 2.273 s |

`jessica` is named by the config but was not re-probed; the audit report
records a live `jessica` call (47 273 B / 2.95 s for 40 chars).

OpenAI's six standard names are **mapped** onto those four (a translation:
OpenAI clients send `alloy`, and forwarding it verbatim would produce the
upstream's nonsense clip when a real voice exists for the intent):

| OpenAI | Upstream |
|---|---|
| `alloy` | `sarah` |
| `echo`, `onyx` | `chris` |
| `fable` | `charlie` |
| `nova`, `shimmer` | `jessica` |

Any other name — including the four upstream names, case-folded — is sent
through unchanged, lowercased and trimmed. An unknown name is therefore the
caller's explicit request to the upstream, not something the gateway silently
swapped. An absent or empty `voice` is sent omitting the field (the upstream's
own default). The README documents the 0.44 s quirk so a client knows what an
unknown name yields.

**Text.** Sent as-is, no length cap (§1.1). The bundle truncates at 5 000
chars client-side; the upstream was not probed above 155 chars, so >5 000 is
undefined and the gateway does not invent a limit.

**Body read.** Complete body, bounded by `IL2API_AUDIO_TIMEOUT` on the whole
call — headers *and* body, because the upstream answers `200` and then streams
— plus a whole-body read cap of 32 MiB. An expiry during the body read is
reported as `upstream_idle_timeout` (504), the audio analogue of the chat
idle window. Body bytes are proxied unchanged — no re-encoding, no ID3
rewriting. The 32 MiB cap is memory safety on the gateway, not a claim about
the upstream's limits; it is 25 000× the largest real clip observed.

**The upload cap is a gateway limit, not the upstream's.** Probe 6 of §2.1.2
sent a 2.5 MB MP3 to `/api/stt`: the upstream answered `200` with a transcript
after 30.9 s — it neither 413'd nor hung. The upstream's own limit is
therefore at least 2.5 MB and of unknown size, so a gateway cap cannot be
justified as "the upstream answers first". `IL2API_AUDIO_MAX_BYTES` (default
25 MiB) is kept for two concrete reasons, both about the gateway's own
resources: (a) the upload is buffered in memory before it is forwarded and
this process has a finite heap, and (b) at the measured ~12 s/MB forwarding
25 MB takes ~5 minutes, longer than the 120 s `IL2API_AUDIO_TIMEOUT`, so a
larger body would only time out after spending the memory. A body over the
cap is `413 request_too_large` before any upstream call, and the SPEC says
plainly that this is the gateway's number (§7.3, §8).

#### 2.5.2 STT — `POST /api/stt`

Multipart, exactly one field `file`; the part **must** carry an explicit
`audio/*` content type (`application/octet-stream` → `400
{"error":"Invalid file type"}`). The byte stream must be real MPEG audio:
WAV at 8/16/48 kHz and WebM-labelled WAV all answer `200 {"text":""}` — a
silent no-op, not an error. A TTS MP3 round-trips byte-exactly
(`outH/stt-tts-loop`). No `model`, `language`, or `prompt` field exists.

**Gateway contract: MP3-only, no transcode.** The gateway sniffs the uploaded
bytes for an MPEG frame sync or an ID3v2 tag and types the part `audio/mpeg`
when found, `application/octet-stream` otherwise, then forwards the bytes
unchanged. It never shells out to ffmpeg and adds no dependency. Non-MPEG
audio is forwarded honestly and the upstream answers `{"text":""}` (or
`Invalid file type`); the gateway does not manufacture a 415 and does not
transcode. This is documented as the upstream's contract, not a gateway
limitation to work around.

Sniff rule: an ID3v2 magic (`ID3`) or a valid MPEG audio frame header
(`0xFF` followed by `0xE0` mask with a legal version/layer/bitrate) within the
first 8 KiB (ID3 tags can precede the first frame) counts as MPEG.

**Empty text is the upstream's answer.** A `200 {"text":""}` is returned to
the client as `{"object":"transcription","text":""}`; the gateway does not
turn it into an error. (The audit suggested mapping empty to a real error;
that would second-guess a legitimate "no speech" answer and is rejected here
under §1.1.) The README documents the MP3-only condition loudly instead.

#### 2.5.3 Errors

Both endpoints reuse the chat path's upstream mapping: non-200 →
`502 upstream_error` carrying the upstream message; pool empty → `503`;
clearance failure → `503`; client gone → no write. Structural failures are
the only gateway-generated 4xx: empty/non-string `input`, missing multipart
`file`, empty file bytes, malformed JSON, an upload over the gateway's
documented cap, method not allowed. An unknown `voice` is **not** one of
them: it is forwarded (§2.5.1).

## 3. Wire format — OpenAI Chat Completions

### 3.1 Request

Decoded fields:

| Key | Handling |
|---|---|
| `model` | required; must be in the configured model list, else `404 model_not_found` (§1.1: the one protocol-level rejection) |
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
4. finish frame: `delta:{"annotations":[…]}` when web-search sources were
   collected (§2.4), otherwise `delta:{}`; `finish_reason:"stop"`
5. `data: [DONE]`

Every frame carries `id` (`chatcmpl-<12 hex>`, stable per request), `object:
"chat.completion.chunk"`, `created` (Unix seconds), `model`, and
`choices:[{index:0,delta:...,finish_reason:null|"stop",logprobs:null}]`.

`finish_reason` is `"stop"` when the upstream stream ends normally and is
never `"length"` (there is no output cap). `source-url` events are translated
into `url_citation` annotations carried on the finish frame (§2.4); the
`__searching__` sentinel is a no-op and `__search_error__` is logged while the
answer keeps flowing. `reasoning-start` / `reasoning-end` / `text-end` produce
no frame of their own.

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
    "message": {"role": "assistant", "content": "...", "reasoning_content": "...",
                "annotations": [{"type": "url_citation", "index": 1,
                                 "url": "https://…", "title": "…",
                                 "start_index": 0, "end_index": 0}]},
    "finish_reason": "stop",
    "logprobs": null
  }]
}
```

`message.annotations` is omitted when the upstream sent no sources; the array
is in arrival order (§2.4).

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

`/v1/web_search`'s terminal `error` event is treated the same way: the
upstream's message rides in the envelope (bounded to 1 000 chars). The
`upstream_stream_error` code remains for a body that closes before `docs`, a
shape failure the upstream did not describe.

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
- **One authed-call wrapper** (`postAuthed`) is the single place a token is
  acquired, attached and retried. On HTTP 401/403 it discards that token,
  gets a fresh one and retries, at most **2 retries** (3 attempts total);
  chat, search, TTS, STT and the config probe all call this wrapper — no
  family posts with a token itself. On a `429`/`403` challenge `Send` retires
  the ring slot once and replays the request (§2.1.1). Other non-200 statuses
  return a typed `*StatusError`; the token is kept.
- **One body-owning layer**: `do` attaches content type, UA and the ring
  cookie, arms the pre-response deadline, and wraps the body so the deadline
  context is released on `Close`. Per-family code supplies only the path,
  bytes, decoder and extra headers (§6.1).
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

### 6.1 Internal architecture (the shared pieces)

The five feature passes (chat → web_search → audio → citations → review) each
arrived with their own copy of the same plumbing. After the Part 3 audit the
code has exactly one implementation of each of these, and this section is the
map. The rule is that a family may add a *decoder* and a set of *headers*,
never a request path.

| Concern | Single home | Per-family input |
|---|---|---|
| Transport, UA, proxy, redirects, h2 | `clear.Transport` + `Client.hc` | — |
| Ring cookie selection and UA application | `Clearance.Header()` called from `Client.do` | — |
| Pre-response deadline (dial + TLS + headers) | `Client.do` | `headerTimeout` |
| Token acquisition + 401/403 retry (≤2) | `Client.postAuthed` | path, label, body, extra headers |
| Challenge retire + one replay | `Client.Send` | — |
| Sliding idle watchdog (SSE / NDJSON) | `upstream.lines` (`reader.go`) — one scanner goroutine + one sliding timer, parameterised by the "is this a data line" predicate and the per-line parser | chat: `data:` framing; search: NDJSON framing |
| Absolute run cap | `upstream.lines` caller passes a `hardCap` (search: 10 idle windows; chat: none) | search only |
| Whole-call timeout (audio) | `upstream.withCallTimeout` | timeout |
| Upstream → envelope mapping (non-stream) | `server.upstreamError` | — |
| Upstream → SSE error frame | `server.streamError` (same switch, same codes) | — |
| Text-shape rules (http URL, control chars, whitespace, rune truncation, rune length) | `upstream/text.go`: `isHTTPURL`, `hasControl`, `collapseSpace`, `truncateRunes` | search and citations share them |
| Source acceptance (URL scheme, title cleanup, count cap) | `upstream.normalizeSources`, used by both the search collector and the citation collector's source path | different caps |
| SSE/JSON writing | `server.writeEvent`, `server.writeJSON` | — |
| Envelope construction | `openai.New` / `openai.Invalid` / `openai.WriteError` | — |
| Constants | `openai.Code*` for every `code`; per-package numeric constants live next to the rule they bound | — |

**Non-streaming error mapping is one function.** `upstreamError` owns the
`upstream.Err*` → status/code table (§3.4) and the `*StatusError` →
`502 upstream_error` translation for **every** route. `streamError` is the
mid-stream variant of that same table (it differs only in how it is written:
an SSE frame with the `upstream_stream_error` code). A per-family mapper
(search once had its own) is a defect: it is how `search_unavailable` and a
duplicate `upstream_errors` increment drifted in.

**The watchdog is one mechanism.** Chat (SSE) and search (NDJSON) are both
"read lines from a body, reset a timer per line, parse some of them, stop at a
sentinel or a hard cap". `lines` implements exactly that; `Reader` (chat) and
`collectSearch` supply the framing predicate and the event handler. Audio is
*not* a stream (one complete body), so it uses the deadline-context form
instead — the two mechanisms are documented side by side here so nobody adds a
third.

**Config plumbing is one hop.** `config.Config` → `upstream.Config` /
`server.Config` → the type that reads the value. No handler holds its own
copy of a default: defaults live in the package that consumes them
(`clear.DefaultRefresh`, `server.defaultAudioTimeout`, …) and `config.Defaults`
mirrors them for startup validation. The action for a new knob is: add it to
`config` (parse + validate), pass it down, give it a consumer-side default,
and add a row to §8 — in that order.

**Tests stay per-family (black-box through the HTTP handler or the `Client`)
plus unit tests for the shared pieces** (`lines`, `isHTTPURL`/`truncateRunes`,
`postAuthed` via a mock upstream). A test that has to reach into a shared
helper to prove family behaviour is a sign the helper's seam is wrong.

## 7. HTTP server

| Route | Method | Auth | Behaviour |
|---|---|---|---|
| `/v1/chat/completions` | POST | yes | §3 |
| `/v1/web_search` | POST | yes | §7.1 |
| `/v1/audio/speech` | POST | yes | §7.2 |
| `/v1/audio/transcriptions` | POST | yes | §7.3 |
| `/v1/models` | GET | yes | static list, `{object:"list",data:[{id,object:"model",created,owned_by:"inceptionlabs"}]}` — audio models are **not** listed (OpenAI does not list them either) |
| `/healthz` | GET/HEAD | no | `{"ok":true,"pool":{"size","min","max"},"clearance":{"has_cookie","age_seconds","refresh_count","slots":[{"cookie","age_seconds","refresh_count"}]},"search":{"available":bool|null},"counters":{...}}` |

### 7.1 `POST /v1/web_search`

Bridges §2.3 into the response contract used by Sliverkiss/simple-chat
(pinned by `TestWebSearchResponseShape` in
`app/internal/server/search_test.go`), so a client written for that gateway
can switch base URL without changing its parser:

```json
// request
{"query": "latest tech news",
 "config": {"fanout": 1, "depth": 1, "keep_top_k": 3, "effort": "instant"}}
```

`config` is optional. `fanout` defaults to `3`, `depth` to `1`, `keep_top_k`
to `10`, `effort` to `instant`; `fanout`/`depth` are clamped to the upstream's
published 1–5 / 1–3 ranges — clamping is a translation, not a rejection.
`keep_top_k` is passed through unchanged: the upstream publishes **no** range
for it (`/api/search-lab/config` has `limits` for fanout and depth only), so a
gateway range would be an invented number. `effort` is passed through for the
same reason: the upstream owns its enum. Unknown top-level and `config` keys
are **silently dropped** (§1.1).
A missing or non-string `query` is a structural 400; an empty or
whitespace-only `query` is a 400. There is no query-length limit beyond the
body cap.

The `retrievalAvailable` probe is **advisory only**: it feeds
`/healthz.search.available` and nothing else. A request always reaches the
upstream, so a stale or wrong probe (the v4 failure class: a probe that
misread a challenge as `false`) can never take a working endpoint offline;
the upstream's own answer — including its error — is what the client sees.
`IL2API_SEARCH_CONFIG_CHECK=off` therefore only silences the health field.

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
| upstream non-200 | 502 | `upstream_error` |
| terminal NDJSON `error` event | 502 | `upstream_error` (upstream message preserved) |
| NDJSON body ends without `docs`/`error` | 502 | `upstream_stream_error` |
| sliding 60 s idle watchdog (any line resets it) | 504 | `upstream_idle_timeout` |

There is no `search_unavailable` 503 any more: the config probe no longer
gates requests (§7.1 preamble). The code stays in the vocabulary for clients
that branch on it, but no path emits it.

### 7.2 `POST /v1/audio/speech`

OpenAI's speech request shape, restricted to the fields the upstream can
honour:

```json
{"model":"tts-1","input":"Hello from the gateway.","voice":"alloy",
 "response_format":"mp3","speed":1.0}
```

| Field | Behaviour |
|---|---|
| `model` | **accepted and ignored**. There is one upstream voice engine; any value is fine (OpenAI SDKs send `tts-1`/`tts-1-hd`). Missing is fine too. |
| `input` | **required**; omitted, non-string, or whitespace-only → `400` with `param:"input"`. Any length is forwarded unchanged. |
| `voice` | OpenAI names mapped onto upstream voices (§2.5.1). Case-insensitive, trimmed, lowercased. An unknown name is **forwarded unchanged** — the upstream answers it (a short clip for a name it does not know). Absent/empty → upstream default (field omitted). |
| `response_format` | **accepted and ignored**. The upstream produces MP3 only; the response is **always** `audio/mpeg`. A client asking for `wav` gets MP3, documented in the README and SPEC rather than a 400 — there is nothing else the upstream can produce. |
| `speed` | **dropped silently**. The upstream has no speed knob. |
| unknown keys | **dropped silently** (§1.1). |

Response (200): the upstream MP3 body, byte-for-byte, with
`Content-Type: audio/mpeg`, `Cache-Control: no-store`, and `Content-Length`.
The body is buffered to completion before headers are committed, so an
upstream failure still yields a proper JSON error envelope and never a
truncated audio stream.

Error mapping:

| Condition | HTTP | code |
|---|---|---|
| malformed JSON / trailing data / not an object | 400 | `invalid_request_error` |
| missing, non-string, or whitespace-only `input` | 400 | `invalid_request_error` (`param:"input"`) |
| method not allowed | 405 | `method_not_allowed` (`Allow: POST`) |
| pool empty | 503 | `upstream_unavailable` |
| clearance failure | 503 | `upstream_clearance_failed` |
| upstream non-200 | 502 | `upstream_error` (upstream message preserved) |
| upstream timeout | 504 | `upstream_idle_timeout` |

`/healthz` counters gain `tts` and `stt` — the number of accepted (method
allowed) requests to each route, incremented at entry like `requests`.

### 7.3 `POST /v1/audio/transcriptions`

OpenAI's transcription request: `multipart/form-data` with a `file` part, plus
optional fields the upstream does not understand.

| Field | Behaviour |
|---|---|
| `file` | **required**. Missing part, empty body, or a part that cannot be read → `400` (`param:"file"`). The bytes are forwarded unchanged, typed by sniffing (§2.5.2). |
| `model`, `language`, `prompt`, `temperature`, `response_format`, unknown fields | **dropped silently**. The upstream accepts none of them. |
| `response_format` | OpenAI's default is `json`; the gateway always returns `json`. `text`/`verbose_json`/`srt`/`vtt` are **not** implemented — the field is dropped. |

Response (200), OpenAI `json` shape:

```json
{"object":"transcription","text":"The quick brown fox jumps over the lazy dog."}
```

`text` is the upstream `text` verbatim (including `""`). `Cache-Control:
no-store` — transcripts derive from the caller's audio.

Error mapping:

| Condition | HTTP | code |
|---|---|---|
| not multipart / missing `file` / empty file | 400 | `invalid_request_error` (`param:"file"`) |
| multipart malformed / body over cap | 400 / 413 | `invalid_request_error` / `request_too_large` |
| method not allowed | 405 | `method_not_allowed` (`Allow: POST`) |
| pool empty / clearance failure | 503 | `upstream_unavailable` / `upstream_clearance_failed` |
| upstream non-200 (e.g. `Invalid file type`) | 502 | `upstream_error` (upstream message preserved) |
| upstream 200 with unparseable JSON | 502 | `upstream_error` |
| upstream timeout | 504 | `upstream_idle_timeout` |

**Upload bound.** Unlike the JSON routes, the transcription body is bounded at
`IL2API_AUDIO_MAX_BYTES` (default 25 MiB), because audio files are legitimately
large and the 6 MiB JSON cap would reject ordinary recordings. This is the
gateway's own limit, not the upstream's: probe 6 of §2.1.2 showed the upstream
accepting 2.5 MB (and taking ~31 s to do it), so the cap is documented as a
memory/latency bound (§2.5.1), not as a translation of an upstream rule.
Exceeding it is a `413` before any upstream call.

`/healthz` gains `search.available`: `true` / `false` from the last
`/api/search-lab/config` probe, `null` before the first one completes or when
probing is disabled. The probe runs once at startup in a background goroutine
(bounded by `IL2API_SEARCH_PROBE_TIMEOUT`, default 15 s) and therefore never
blocks boot; a failed probe leaves the value `null`. The probe is **advisory
only**: no request is ever gated on it (§7.1), so a stale or wrong value
cannot disable a working endpoint. `IL2API_SEARCH_CONFIG_CHECK=off` skips the
probe entirely.

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
| `IL2API_CLEARANCE_REFRESH` | `45m` | proactive clearance refresh age **per ring slot** (cookie TTL is 60m) |
| `IL2API_CLEARANCE_COOLDOWN` | `15s` | minimum retry interval after a **failed** clearance acquisition |
| `IL2API_CLEARANCE_POOL` | `3` | clearance cookies held in the rotation ring (§2.1.2); `1` = the old single-cookie behaviour |
| `IL2API_SESSION_TIMEOUT` | `15s` | single `/api/session` fetch timeout |
| `IL2API_SEARCH_TIMEOUT` | `60s` | `/v1/web_search` sliding idle watchdog (the same window the chat path uses for `IL2API_IDLE_TIMEOUT`) |
| `IL2API_SEARCH_CONFIG_CHECK` | `on` | `on`/`off`; `off` skips the startup `/api/search-lab/config` probe (`search.available` stays `null`). The probe is advisory; requests are never gated on it |
| `IL2API_SEARCH_PROBE_TIMEOUT` | `15s` | bound on the one startup config probe |
| `IL2API_AUDIO_TIMEOUT` | `120s` | whole-call timeout for `/api/tts` and `/api/stt` (headers and body; TTS took ~1 s live, the window is generous for long inputs) |
| `IL2API_AUDIO_MAX_BYTES` | `26214400` (25 MiB) | gateway upload cap for `/v1/audio/transcriptions` (memory/latency bound, §2.5.1); larger bodies are a 413 without an upstream call |

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
  (`/healthz` + one chat + one search) described in the task. Live probing is
  limited to the documented probe budget: the voice-mapping probes (§2.5.1),
  the six clearance-ring probes and the 2.5 MB STT probe (§2.1.2), and the
  final three-request smoke.
- Integration tests run the real HTTP handlers against an `httptest` mock
  upstream, covering streaming framing, reasoning ordering, multi-block
  streams, snapshot text, mid-stream failure, idle timeout, auth, oversized
  input pass-through, dropped params, tools-as-plain-chat, media placeholder,
  and client disconnect.
- Citation translation is pinned against scripted `source-url` streams:
  normal sources (rank order, annotation shape, finish-frame delivery),
  `__searching__` as a no-op that consumes no rank, `__search_error__`
  logged without aborting the text, missing/empty URLs, non-http schemes,
  duplicate URLs, sources after `text-end` followed by a later text block,
  the capture fixture `testdata/source-url-capture.sse`, and the non-stream
  `message.annotations` shape. The sentinel titles never reach a client.
- Clearance prewarm is pinned: boot starts acquisition without blocking
  (a stalled challenge does not delay `StartClearancePrewarm`'s return), a
  failed acquisition is tolerated and leaves `has_cookie:false`, and the
  refresh loop re-acquires at the configured age from boot.
- The clearance **ring** (§2.1.2) is pinned against a mock challenge endpoint:
  boot fills every configured slot; `Header()` rotates once per request
  (`n` requests over `k` slots each see each slot `n/k` times); a slot past
  the refresh age is skipped while a live slot exists; the refresh loop
  replaces one stale slot per tick; a 429 retires only the challenged slot
  and leaves the others' ages untouched, then refills in the background; the
  failed-acquisition cooldown is ring-wide; `IL2API_CLEARANCE_POOL=1`
  reproduces the single-cookie behaviour. `/healthz` reports per-slot ages and
  refresh counts.
- Audio is tested against `httptest` mocks of `/api/tts` and `/api/stt`
  through the same TLS-checkpoint mock as chat, so the real
  `postAuthed`/`Send` path is exercised:
  - TTS success (request body shape `{text,voice}`, response bytes returned
    unchanged with `audio/mpeg`), default voice (field omitted), every
    mapped voice, `voice` case folding, an **unknown voice forwarded
    unchanged** to a mock upstream (and the mock's own 200 bytes returned to
    the client — the pass-through assertion that replaced the old 400 test),
    `response_format`/`speed`/`model` ignored, empty input → 400, trailing
    JSON → 400, upstream 4xx → 502 envelope, pool/clearance failures → 503,
    method not allowed.
  - STT success (multipart `file` forwarded byte-for-byte, part typed
    `audio/mpeg` when the bytes are MPEG, `application/octet-stream`
    otherwise), `{object:"transcription",text}` shape, empty text preserved
    as 200 `""`, upstream `Invalid file type` → 502, missing `file` → 400,
    empty file → 400, oversized upload → 413 without an upstream call,
    non-multipart → 400, method not allowed.
  - One full-stack integration test: TTS MP3 → STT text, against a mock
    upstream whose `/api/stt` echoes a known transcript.
- Web search is tested entirely against an `httptest` NDJSON mock: the full
  plan → retrieve → rerank → docs pipeline, a gzip-encoded body, a `br` body
  typed error, a missing `docs` event, a terminal `error` event (the upstream
  message preserved), truncation, upstream non-200, out-of-range
  `fanout`/`depth` clamped, `keep_top_k` **passed through** unclamped, a
  request reaching the mock even when the cached probe says
  `retrievalAvailable:false` (the probe is advisory), the sliding idle
  watchdog, and the startup config probe (success, failure, disabled)
  reflected only on `/healthz`.
- The shared pieces have direct unit tests: the line scanner (`lines`) for
  framing, malformed lines, idle reset and the hard cap; `isHTTPURL` /
  `hasControl` / `collapseSpace` / `truncateRunes`; `postAuthed`'s retry
  ceiling against a mock; and the single error mapper's table, asserted once
  and then relied on by every family's black-box test.
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
