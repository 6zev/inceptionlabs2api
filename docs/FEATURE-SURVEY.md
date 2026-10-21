# Feature-surface survey — what is left to bridge

Date: 2026-10-21 · Branch: `feat/decisions` · Status: **survey only, no
implementation** (decide is the one candidate implemented in this branch;
code-lab/voice are deliberately left for a later decision).

Inputs:

1. **Official API docs**, read in full via `https://docs.inceptionlabs.ai/llms.txt`
   and each page's `.md` source (the OpenAPI pages embed
   `api.inceptionlabs.ai/openapi.json`). Endpoint list below is from that
   spec, not from memory.
2. **Web routes**, from `/root/inception-dig/REPORT.md` §2 (16 `/api/*`
   routes mapped at `chat.inceptionlabs.ai`).
3. **What the gateway already serves** (`README.md`, `docs/SPEC.md` §7):
   `/v1/chat/completions`, `/v1/web_search`, `/v1/audio/speech`,
   `/v1/audio/transcriptions`, `/v1/models`, `/healthz`, plus tool calling
   emulated over the prompt.

## 1. Every official API endpoint (docs.inceptionlabs.ai)

From `https://api.inceptionlabs.ai/openapi.json` — **seven** operations, five
useful paths:

| Method | Path | Model | Streaming | Shape |
|---|---|---|---|---|
| POST | `/v1/decisions` | `mercury-decide` | no | `{model,state,questions{name:{type,instructions,criteria}}}` → `{model,answers,usage}` |
| POST | `/v1/chat/completions` | `mercury-3`, `mercury-voice`, `mercury-2.5`, `mercury-2` | SSE when `stream:true` | OpenAI `ChatCompletion` + extensions (`reasoning_effort`, `diffusing`) |
| POST | `/v1/fim/completions` | `mercury-edit-2` | SSE when `stream:true` | `{model,prompt,suffix,max_tokens}` → `{choices:[{text}]}` |
| POST | `/v1/edit/completions` | `mercury-edit-2` | **no** | OpenAI-shaped `messages` containing `<|…|>` edit tags → `{choices:[{message}]}` |
| GET | `/v1/models` | — | — | OpenAI model list (the gateway already serves its own static list) |
| GET | `/v1/chat/completions/models` | — | — | chat-model subset |
| GET | `/v1/fim/completions/models` | — | — | FIM-model subset |
| GET | `/v1/edit/completions/models` | — | — | edit-model subset |

What the official API does **not** have (explicitly, in the FAQ and docs):
`/v1/embeddings` (none), `/v1/audio/*` (no TTS/STT), `/v1/images/*` (no image
generation), `/v1/responses` (no Responses API), fine-tuning, batch,
`/v1/moderations`, MCP server, `seed`/reproducibility, file uploads. Model
line-up: Mercury 3 (1M context, 131k output, tool calling, structured
outputs), Mercury Voice (128k/50k, enterprise), Mercury Decide (classification
/scoring/extraction, text/JSON/image), Mercury 2.5 (260k/65k), Mercury 2
(128k/50k), Mercury Edit 2 (FIM 32k / next-edit 32k, 8 192 output).

The **web product** offers three surfaces the official API does *not*:
audio (`/api/tts`, `/api/stt`), the voice agent (`/api/voice/*`), and the
code lab (`/api/code-lab/*`). That asymmetry is the whole opportunity: the
gateway rides the web transport, so it can bridge things the keyed public API
never will.

## 2. Cross-reference: web route → official API → gateway

| Web route (REPORT §2) | Official API equivalent | Gateway today | Verdict |
|---|---|---|---|
| `GET /api/session` | none (API-key auth) | internal (token pool) | infrastructure, done |
| `POST /api/chat` | `POST /v1/chat/completions` | **done** (§3, streaming + tools) | done |
| `POST /api/follow-ups` | none | **not implemented** | see §3.4 |
| `POST /api/tts` | none | **done** (`/v1/audio/speech`) | done |
| `POST /api/stt` | none | **done** (`/v1/audio/transcriptions`) | done |
| `GET /api/voice/config` | none | not implemented | see §3.2 |
| `POST /api/voice/turn` | none (Mercury Voice is a chat model, not this agent loop) | not implemented | see §3.2 |
| `POST /api/voice/extract` | partially (`extract` question type) | not implemented | see §3.2 |
| `GET /api/search-lab/config` | none (search cookbook only) | internal (advisory probe) | done |
| `POST /api/search-lab/run` | none | **done** (`/v1/web_search`, sources only) | done |
| `GET /api/decide/config` | none | not implemented (client supplies it) | deliberately not bridged |
| `POST /api/decide/run` | `POST /v1/decisions` | **this branch** (`/v1/decisions`) | done here |
| `GET /api/code-lab/config` | none | not implemented | see §3.1 |
| `POST /api/code-lab/run` | none (coding cookbook only) | not implemented | see §3.1 |
| `POST /api/code-lab/design` | none | not implemented | see §3.1 |
| `POST /api/code-lab/browser` | none | not implemented | see §3.1 |

## 3. Verdict table for each candidate

Legend: **web route** = upstream web endpoint exists and is reachable with a
pooled token; **official API** = a public-API equivalent exists; **natural
surface** = the shape a gateway client would expect; **effort** = engineering
work in *this* codebase (shared plumbing already exists); **value** = who it
serves and whether they can get it elsewhere.

### 3.1 Decide — **implemented in this branch**

- **(a) Web route:** yes — `POST /api/decide/run`, probes `docs/PROBE-decide.md`.
- **(b) Official API:** yes — `POST /v1/decisions` (map `questions` +
  `criteria`, `mercury-decide`), non-streaming.
- **(c) Surface:** upstream **web** shape as-is (array `questions`, answers
  with `p`/`levels`, top-level `inputTokens`/`latencyMs`), per the project
  directive. The public-API map shape is **not** adopted; the differences are
  tabulated in `docs/PROBE-decide.md` §6.
- **(d) Effort:** small (one codec + one handler; reuses `postAuthed`,
  `upstreamError`, the body reader, the 405 generator and the whole-call
  deadline). Value: medium — it is the only route that returns **typed
  probabilities** rather than generated text, and the official `/v1/decisions`
  needs a paid key while the web route needs only an anonymous session.

**Not bridged, deliberately:** `/api/decide/config` (the five scenario
presets). They are a playground convenience: a gateway client supplies its own
state and questions, and pinning 7 KB of upstream demo content would make the
gateway a stale copy of someone else's UI.

### 3.2 Voice agent — `/api/voice/config`, `/api/voice/turn`, `/api/voice/extract`

- **(a) Web route:** yes — all three mapped; `turn` is a live-probed NDJSON
  stream (REPORT §3.3), `extract` is bundle-only, not probed.
- **(b) Official API:** **no** agent endpoint. Mercury Voice is a *chat model*
  (`v1/chat/completions`, enterprise-gated) — it gives voice models, not the
  scenario/tool-loop this route runs.
- **(c) Surface:** upstream-aligned (`POST /v1/audio/chat`-style taking
  `{scenarioId, messages, businessDone, timezone}` and re-emitting the NDJSON
  events as SSE). It is **not** OpenAI-shaped and has no OpenAI analogue
  (`/v1/audio/chat` does not exist in OpenAI's API).
- **(d) Effort:** medium — one NDJSON decoder plus an SSE re-framer over the
  existing line pump; the scenario config must be forwarded, not cached.
  Value: medium. It is the **only** route that exposes `reasoningTokens`,
  in-band TTFT, and tool-call backends — genuinely interesting telemetry — but
  the audience is narrow (voice-agent prototypes), and the scenario DSL is
  upstream's own, so the gateway would be a thin proxy with little to add.
  **Recommendation: defer**; re-evaluate if a voice client asks for it.

### 3.3 Code lab — `/api/code-lab/run`, `/design`, `/browser`

- **(a) Web route:** yes — three routes, all bundle-only (REPORT §3.9), none
  probed live.
- **(b) Official API:** **no** equivalent. FIM/Next-Edit are different
  products (inline completion on a document, not an agent loop over a virtual
  filesystem); the official *coding agent* cookbook is just chat + tools, not
  a route.
- **(c) Surface:** upstream-aligned NDJSON (`step_start`, `write_progress`,
  `tool_start/end`, `file_changed`, `run_done`) for `run`/`browser`, plain
  JSON for `design`. No OpenAI surface fits; a `/v1/code-lab/run` proxy is the
  honest option.
- **(d) Effort:** **large**. Three different event vocabularies, long runs
  (minutes), a virtual-FS request shape, and browser agent state
  (`viewerDid`) the gateway has no way to manage. Value: **low** for a
  gateway whose clients bring their own agent framework and can use chat +
  tools; the upstream itself is the better client for this feature.
  **Recommendation: do not build.**

### 3.4 Follow-ups — `/api/follow-ups`

- **(a) Web route:** yes — probed (`outC/followups`), returns 3 suggested
  questions as JSON.
- **(b) Official API:** no; it is a one-shot chat completion behind a prompt.
- **(c) Surface:** none natural. OpenAI has no "suggested replies" endpoint;
  an OpenAI client can already ask chat for this. Any gateway shape would be
  invented (`/v1/follow_ups` with one provider's request body), which is
  exactly the "second product owner" the pass-through rule forbids.
- **(d) Effort:** small; value: **low** (duplicates chat; one upstream point
  of failure more). **Recommendation: do not build.**

### 3.5 New candidates the docs reveal

| Candidate | Web route | Official API | Verdict |
|---|---|---|---|
| **FIM** (`/v1/fim/completions`) | none (web has no autocomplete UI) | yes, `mercury-edit-2`, SSE optional | **No.** No web upstream at all; the gateway's anonymous transport cannot reach an endpoint that does not exist. Only possible with a paid `INCEPTION_API_KEY`, which is a different product (not this gateway). |
| **Next-Edit** (`/v1/edit/completions`) | code lab's `design` is adjacent but not the same | yes, `mercury-edit-2` | **No**, same reason as FIM. |
| **Model list per family** (`/v1/{chat,fim,edit}/completions/models`) | none | yes | **No.** The gateway already answers `/v1/models` from `IL2API_MODELS`; more lists would be copies. |
| **Structured outputs** (`response_format` json_schema) | the web `/api/chat` has no schema slot | yes, chat extension | **No.** The chat wire has no field to carry it; emulating it would need a prompt codec with no upstream contract. Already documented as dropped (§1.1). |
| **Mercury Voice as a chat model** | none (web voice agent is separate) | yes, enterprise-gated `mercury-voice` | **No.** Enterprise access, not reachable anonymously; it is a model id, not a feature. |
| **Embeddings / images / responses / batch / fine-tune** | none | **none exist** | **No** — nothing to bridge. |
| **`diffusing` chat mode** (stream denoising snapshots) | UI toggle is client-only (`localStorage`, REPORT §4) | yes, chat field | **No.** The web chat wire has no field; the UI toggle never reaches the server. |

## 4. Recommendation

1. **Decide (this branch)** — merge; it is the highest value-per-effort of
   what remains, and the only non-text answer surface.
2. **Voice agent** — the only deferred candidate worth re-opening later. It
   is cheap (one NDJSON decoder on the shared line pump) and exposes data no
   other route does (`reasoningTokens`, TTFT). It needs a product decision on
   the surface name and on whether scenario config is forwarded or curated.
3. **Code lab, follow-ups, FIM, next-edit** — decline. They either have no
   upstream on the anonymous web transport, no natural surface, or duplicate
   chat.

Nothing in this survey changes the current `/v1/*` contract.
