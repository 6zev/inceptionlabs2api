# RESEARCH — How chat.inceptionlabs.ai implements web search

**Date:** 2026-10-21
**Target:** `https://chat.inceptionlabs.ai` (Inception Chat, Mercury, Next.js 16 / Turbopack on Vercel)
**Method:** static analysis of production JS bundles + 3 live probes (21 s apart) from a challenge-cleared browser context
**Status:** research documentation only. No `.go` file was read for modification, and no project code was changed.

---

## 0. Answer in one line

**There is no search endpoint that returns raw ranked results.** Search surfaces through
**two distinct paths**:

1. **Inline, inside `/api/chat`** — `webSearchEnabled: true` makes the server-side agent
   run its own retrieval loop and interleave `source-url` events into the same chat stream.
   There is no separate call, no tool schema, and no forced-search message shape. The model
   decides *whether* to search; that decision is not exposed.
2. **A standalone, product-facing retrieval pipeline at `/api/search-lab/run`** — an
   **NDJSON** streaming endpoint that runs plan → retrieve → rerank → follow-up → synthesise
   and emits structured sources *and* a synthesized answer. It is the backend of the
   public `/search` playground page. It is a **retrieval pipeline with an LLM synthesis
   stage**, not a "results only" search API — but it is callable independently of
   `/api/chat`, and its `docs` event carries `{title, url, score}`.

A third route, **`/api/search`**, exists and answers `POST` with
`{"results":[],"skipped":true}`, but it is **not referenced by any client bundle**. It is
most likely `/api/search` for the chat's internal search tool layer (a "skipped" stub when
called without the internal contract). Treat it as undocumented/unstable.

The four `gateway.*_search` tool schemas (`exa_search`, `parallel_search`,
`perplexity_search`, `tako_search`) found in the bundle are **dead code imported from the
Vercel AI SDK / AI Gateway provider catalogue**. They are registered in a provider map
(`exaSearch`, `parallelSearch`, `perplexitySearch`, `takoSearch`) but **never referenced by
any application code**, never passed to `useChat`, and never appear in a request body.
They must not be mistaken for the product's search implementation.

---

## 1. Evidence base and its limits

### 1.1 Bundle inventory

The site is a Next.js App Router build served with Turbopack chunk names
(`/_next/static/immutable/chunks/<hash>.js`). Two capture sets were used:

| Set | Source | Files | Notes |
|---|---|---|---|
| **LIVE** (primary) | Chromium (Playwright) → `/`, `/search`, `/voice`, fetched in-page with `fetch()` | 15 JS chunks | Full app graph for the current deployment |
| **ARCH** (secondary) | Wayback Machine `id_` raw captures, 2026-10-03…07 | 9 JS chunks + HTML | Used only for cross-checks |

**Chunk identity was verified**: five chunks that exist in both sets are byte-identical
(md5 match) — e.g. `1lb54pdvs4b7p.js` = `514c01e7…`, `0c0hxoamwjsbw.js` = `846118c3…` —
so the archived and live builds are the same deployment lineage. Citations below use the
**live** filenames where the code is present there.

| Live chunk | Role | Archived sibling |
|---|---|---|
| `2wmuhr7tyn5ak.js` (578 KB) | **Main app chunk**: chat transport, `useChat` wiring, AI SDK internals, provider tool catalogue, `__searching__` / `__search_error__` handling | `1ye2f2fzco-j2.js` (675 KB) |
| `3r_9svhrldbt0.js` (32 KB) | **`/search` playground page** (`SearchLab` component) — `/api/search-lab/*` calls | not archived |
| `0zw1816s_o6fw.js` (32 KB) | `/voice` playground page — `/api/voice/*`, `/api/stt`, `/api/tts` | `0zw1816s_o6fw.js` (same) |
| `1bphncyfur5lp.js` (23 KB) | `SessionProvider` — `/api/session`, `sessionHeaders` | `1bphncyfur5lp.js` (same) |
| `3fuz_l69h_zsk.js`, `22d8u9d04wag_.js`, `3i5963btmjtx1.js`, `2969v_5aik-bo.js`, `1kko-itdalfly.js` | framework / route loaders / nav | — |

**Gaps.** The chat message renderer (`MessageList`, module id `77783`) and
`ArtifactPanel` (`16161`) are lazily loaded and were **not** captured by either method.
Consequently the exact **visual affordance** for sources and the exact **"searching…"
copy** cannot be quoted; what they consume is fully visible (see §3.3), and the toggle
button's ARIA label is in the main chunk.

### 1.2 How the site was reached (transport note)

From this host, plain `curl` and the project's utls fingerprint transport still receive
Vercel's challenge (`HTTP/2 429`, `x-vercel-mitigated: challenge`) on **every** path,
including `/_next/static/` and `/favicon.ico`. A headed-ish Chromium via Playwright
(`--no-sandbox --disable-blink-features=AutomationControlled`, Chrome UA) passes the
challenge transparently; after that, in-page `fetch()` to same-origin endpoints works
normally. This matches the audit's §0.1 finding and the audit's own `probe_chat.py`
approach. **All live evidence in §5 was collected this way.**

### 1.3 Probe budget

Exactly **3** live network probes, spaced 21 s:

| # | Request | Purpose |
|---|---|---|
| 1 | `GET /api/search-lab/config` | Confirm the endpoint and its response schema |
| 2 | `POST /api/search-lab/run` (fanout 1, depth 1) | Capture the NDJSON wire format end-to-end |
| 3 | `POST /api/search` `{"query":"test"}` | Negative control — confirm whether a generic route exists |

No chat probes were issued (the chat request shape is already established by the audit and
by the bundle).

---

## 2. Complete inventory of request paths

Every URL-like literal in the live application chunks (grep of `fetch("…")` and of
`"/api/…"` / `"/path"` string literals), with its call site:

| Path | Method | Call site (file · symbol) | Body / return | Search-related? |
|---|---|---|---|---|
| `/api/chat` | POST | `2wmuhr7tyn5ak.js` · `new v5({api:"/api/chat", headers:()=>br.sessionHeaders(), fetch: …})` — `v5` is the AI SDK `DefaultChatTransport`; wired into `useChat({transport:u, experimental_throttle:50, onError, onFinish, …})` | AI SDK UI-message request: `{id, messages:[{id,role,parts}], trigger, …body}`; response is the UI message stream | **Yes** — `webSearchEnabled` triggers inline search (§3) |
| `/api/search-lab/config` | GET | `3r_9svhrldbt0.js` · `fetchPlaygroundConfig("/api/search-lab/config")` → module `60525` `async function c(e,t=0){ let r=await fetch(e,{headers:sessionHeaders()}); if(401===r.status && t<6) retry after 300 ms; if(!r.ok) throw Error("config unavailable"); return r.json() }` | JSON capabilities + defaults (§4.2) | **Yes** — feasibility gate |
| `/api/search-lab/run` | POST | `3r_9svhrldbt0.js` · `SearchLab` → `fetch("/api/search-lab/run",{method:"POST", headers: sessionHeaders({"Content-Type":"application/json"}), body: JSON.stringify({query, config, timezone, language}), signal})` → `readNdjson(e.body, o)` (module `10271`) | NDJSON stream, one JSON object per line (§4.3) | **Yes** — the standalone retrieval pipeline |
| `/api/search` | POST | **No client reference.** Discovered by probe only | `{"results":[],"skipped":true}` (200) | **Possibly** — internal tool layer, undocumented |
| `/api/session` | GET | `1bphncyfur5lp.js` · `SessionProvider` → `fetch("/api/session")`; stores `token`; refreshes every `78e4` ms = **780 s (13 min)**; `sessionHeaders(e)` adds `x-session-token` | `{ok, token}` | Auth for all of the above |
| `/api/follow-ups` | POST | `2wmuhr7tyn5ak.js` · chat hook → `fetch("/api/follow-ups",{method:"POST", headers: sessionHeaders({"Content-Type":"application/json"}), body: JSON.stringify({messages: messages.map(({role,parts})=>({role,parts}))})})` | `{follow_ups: string[]}` | No (suggested next questions) |
| `/api/stt` | POST | `2wmuhr7tyn5ak.js` (×2) and `0zw1816s_o6fw.js` · `FormData` field `file` = `audio.webm` | `{text}` | No |
| `/api/tts` | POST | `2wmuhr7tyn5ak.js`, `0zw1816s_o6fw.js` · `{text: text.slice(0,5000), voice?}` | audio | No |
| `/api/voice/config` | GET | `0zw1816s_o6fw.js` · `fetchPlaygroundConfig("/api/voice/config")`; reads `scenarios[]` | JSON | No |
| `/api/voice/turn` | POST | `0zw1816s_o6fw.js` · `{scenarioId, messages, businessDone, timezone}` | NDJSON-ish `{t:"delta"\|"preface"\|…}` | No |
| `/api/voice/extract` | POST | `0zw1816s_o6fw.js` · `{scenarioId, messages}` | JSON | No |
| `/search` | GET | `2969v_5aik-bo.js` and the inline nav in `2wmuhr7tyn5ak.js` · `{href:"/search",label:"Search",blurb:"Watch a retrieval pipeline run"}` | page | **Yes** — UI for `/api/search-lab` |
| `/voice` | GET | same nav array · `{href:"/voice",label:"Voice",blurb:"Call a realtime voice agent"}` | page | No |

**No server actions were found.** Grep for `$ACTION_ID`, `createServerReference`,
`next-action`, and `serverReference` across all 24 chunks (live + archived) returned
nothing. The app talks to route handlers only, via `fetch`.

**No route manifest is exposed** in the archived HTML (no `_next/static/…/_buildManifest.js`
reference; App Router + Turbopack). Therefore the absence of `/api/search` from the bundles
does not prove it is unreachable — and the probe proves it *is*.

---

## 3. Inline search: `/api/chat` + `webSearchEnabled`

### 3.1 Request shape

The chat hook is created once, at chat state initialisation:

```js
// 2wmuhr7tyn5ak.js — chat state hook (archived equivalent: 1ye2f2fzco-j2.js)
let [reasoningEffort, setReasoningEffort] = useState("medium"),
    [webSearchEnabled, setWebSearchEnabled] = useState(true),   // default ON
    [voiceMode, setVoiceMode]                 = useState(false);
…
const transport = useMemo(() => new v5({
  api: "/api/chat",
  headers: () => sessionHeaders(),
  fetch: async (url, init) => { /* strip user-agent; retry up to 2× on 429 after 1500 ms */ }
}), []);
```

and every send attaches the same five-field body:

```js
// 2wmuhr7tyn5ak.js — sendMessage
b.sendMessage({ text }, { body: {
    reasoningEffort,                 // "low" | "medium" | "high"   (default "medium")
    webSearchEnabled,                // boolean                     (default true)
    voiceMode,                       // boolean                     (default false)
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    language: navigator.language
}});
```

The transport adds the AI SDK envelope on top: `{ id, messages, trigger }` — plus the
above body spread. The audit's captured body (`docs/SPEC.md §2.2`) is exactly this:
`reasoningEffort, webSearchEnabled, voiceMode, timezone, language, id, messages, trigger`.

Notes that matter for a gateway:

- **Header:** `x-session-token` is attached by `sessionHeaders()` only, never `Authorization`.
  There is **no** cache-busting nonce and no `web-search: 1` style header.
- **No forced-search affordance exists.** There is no `forceSearch`, `searchQuery`,
  `searchMode`, `tool_choice`-equivalent, or pre-search message shape anywhere in the
  body builder. `webSearchEnabled` is a single boolean and nothing else.
- **The model, not the client, decides whether a search happens.** The UI treats search as
  an *outcome*: `isSearching = !!hasSentinel && !hasAnyText && isStreaming`
  (`2wmuhr7tyn5ak.js`). It shows a searching state only after the server has already
  emitted the sentinel — the client cannot request one.
- **`diffusionEnabled` is not sent** in this build. It is UI state (`showDiffusionToggle`)
  but never reaches the request body (grep of `body:{…}` shows only the five fields).
- **Client retry policy:** on HTTP 429 it retries the identical request up to 2 more times
  at 1.5 s / 3 s, after deleting `user-agent` from the headers object. Anything else is
  returned as-is.
- **Defaults differ from a gateway's usual default.** Web search is **on by default** for
  a new chat, and the toggle is a plain client-side boolean forwarded verbatim.

### 3.2 Event vocabulary (with search on)

From the audit's captured stream (`/tmp/audit-inception.md` §1.1–1.2, the authoritative
raw capture; the original `out_web.json` scratch file no longer exists):

```
reasoning-0 → text-0 → 13× source-url → reasoning-1 → text-1 → [DONE]
```

**12 real sources + 1 sentinel** = 13 `source-url` events. The stream is a Vercel AI SDK
**UI message stream** (`x-vercel-ai-ui-message-stream: v1`), `data: {json}\n\n` per event,
terminated by `data: [DONE]`. Full vocabulary: `reasoning-start` / `reasoning-delta` /
`reasoning-end` / `text-start` / `text-delta` / `text-end` / `source-url` / `[DONE]`. There
is **no** `tool-*`, `start-step`/`finish-step`, `error`, `usage`, or `finish-*` event, and
no `annotations` field.

### 3.3 Sentinel semantics — two magic titles, not one

The client parses two reserved `title` values on `source-url` parts. Both are
**client-visible protocol**, and both should be filtered by any consumer that maps
`source-url` → citations.

```js
// 2wmuhr7tyn5ak.js — chat state hook (archived: 1ye2f2fzco-j2.js)
const k = messages.findLast(m => m.role === "assistant");
const urls = k?.parts.filter(p => p.type === "source-url");
const I = urls?.some(p => p.title === "__searching__");                 // sentinel
const S = (k?.parts.filter(p => p.type === "text") || [])
            .some(p => "text" in p && p.text.length > 0);               // any answer text yet?
const j = status === "streaming" || status === "submitted";
// isSearching
I && !S && j
```

```js
// searchResultsMap: real citations, sentinel excluded
for (const m of messages) {
  if (m.role !== "assistant") continue;
  const r = m.parts.filter(p => p.type === "source-url"
                             && p.title !== "__searching__"
                             && !!p.url);
  if (r.length) out[m.id] = r.map(p => ({ title: p.title || "", url: p.url, snippet: "" }));
}

// searchErrorSet: assistant messages whose stream reported a search failure
const O = new Set();
for (const m of messages)
  if (m.role === "assistant"
      && m.parts.some(p => p.type === "source-url" && p.title === "__search_error__"))
    O.add(m.id);
```

Machine-checkable facts:

- `__searching__` is emitted **as a `source-url` event** and marks "search phase started".
  The UI shows a searching indicator while it is present **and no text has been emitted
  yet** — i.e. the sentinel precedes the answer and is superseded by text.
- `__search_error__` is emitted **as a `source-url` event** too, and marks a failed search
  for that assistant message. This event is **not mentioned in the audit** (the audited
  stream was a successful search); it is a second reserved title that a citation mapper
  must handle. Recommended mapping: drop it from citations **and**, optionally, surface it
  as a warning rather than silently pretending search succeeded.
- Real sources are those with `title !== "__searching__" && title !== "__search_error__"`
  and a non-empty `url`. The client's own filter only excludes `__searching__`, which
  means **`__search_error__` currently leaks into the client's `searchResultsMap` with
  `url` set to whatever the server sent** — a client bug worth knowing but not something
  a gateway must replicate.
- The UI never shows a per-source snippet: `snippet` is hard-coded to `""`.
- The assistant message id is the map key: `searchResultsMap[message.id]` — sources are
  attributed per assistant turn, so a multi-turn chat can have several source sets.

### 3.4 Answers to the specific questions

| Question | Answer | Evidence |
|---|---|---|
| Does the model decide when to search? | **Yes.** No client control beyond the boolean; there is no `force`/`required`/query field. | §3.1 — the only body field is `webSearchEnabled` |
| Is there a forced-search message shape? | **No.** No system-prompt injection, no special user-part type, no `search` part in the UI message schema. | no `type:"search"` in the AI SDK part schema (`source-url`, `source-document`, `file`, `reasoning`, `text`, `step-start`, dynamic-tool parts only) |
| Does `webSearchEnabled:false` guarantee no search? | **Probably, but not proven.** The client default is `true`; the flag is passed straight through. The audit only observed sources with `true`. | §3.1; audit §1.1 |
| How many sources? | 12 real + 1 sentinel in the audited run. Not a fixed contract — it is whatever the server retrieval returned. | audit §1.1 |
| Are source events ordered? | Sources arrive after the first text block and before the second reasoning block; they are emitted as a contiguous burst. | audit §1.1, §1.5 (sources at 628 ms–2.4 s) |

---

## 4. Standalone retrieval: `/api/search-lab/*`

### 4.1 What it is

The `/search` page ("Search Playground", `<title>Search Playground · Inception</title>`,
nav blurb *"Watch a retrieval pipeline run"*) is a first-party playground over a real
retrieval pipeline. Its UI exposes exactly three knobs:

- **Parallel web searches** (labeled `Parallel web searches`, "Queries written from your
  question, all run at once") — options `1..5`
- **Iterations** ("Rounds of searching, each waiting on the one before, stopping as soon
  as nothing is missing") — options `1..3`
- **Run / Stop**

Pipeline stages, from the `SearchLab` stage map in `3r_9svhrldbt0.js`:

| Kind | Label | Purpose (verbatim) | LLM? |
|---|---|---|---|
| `plan` | Plan | "Rewrites the question into several searches a literal match would miss." | yes |
| `retrieve` | Retrieve | "Runs every planned query against the index at once, then merges and dedupes." | no |
| `rerank` | Rerank | "Scores the merged candidate set against the original question, across every query's results at once." | yes |
| `followup` | Follow up | "Reads what came back, decides whether anything material is still missing, and writes the queries for the next round." | yes |
| `synthesize` | Synthesise | "Writes the grounded answer the user reads." | yes |

Client defaults (`p` in `3r_9svhrldbt0.js`): `{fanout:4, depth:2, keepTopK:5, effort:"instant"}`.
The same defaults are echoed by the server in `/api/search-lab/config`.

This is **not** a "search results only" API: it always synthesises an answer. But the
`docs` event carries the candidate set *before* synthesis, so a caller that wants raw
results can stop reading at `docs` and `AbortController.abort()`.

### 4.2 `GET /api/search-lab/config`

Request: no body. Headers: `x-session-token` (client sends it; whether the server requires
it is unverified — the probe sent none and got 200, see below). The client retries on 401
up to 6 times with 300 ms spacing and throws `"config unavailable"` on other non-2xx.

**Live response (probe 1, 200, 196 ms, `content-type: application/json`, `x-matched-path: /api/search-lab/config`):**

```json
{
  "modelLabel": "Mercury",
  "retrievalAvailable": true,
  "defaults": { "fanout": 4, "depth": 2, "keepTopK": 5, "effort": "instant" },
  "examples": [
    "How have central banks changed interest rates so far this year?",
    "Which US states have passed right-to-repair laws, and what do they cover?",
    "Which countries have trialled a four-day working week, and what did the results show?",
    "Which films won the top awards at the most recent Cannes Film Festival?"
  ],
  "limits": { "minFanout": 1, "maxFanout": 5, "minDepth": 1, "maxDepth": 3 }
}
```

`retrievalAvailable:false` is the deployment kill-switch; the client disables Run and
shows *"Web retrieval is not configured on this deployment."*

### 4.3 `POST /api/search-lab/run`

**Request**

```
POST https://chat.inceptionlabs.ai/api/search-lab/run
Content-Type: application/json
x-session-token: <token>          # sent by the client; not required in the probe
Accept: */*
```

```json
{
  "query": "Which countries have trialled a four-day working week, and what did the results show?",
  "config": { "fanout": 1, "depth": 1, "keepTopK": 3, "effort": "instant" },
  "timezone": "UTC",
  "language": "en-US"
}
```

- `config` is optional; omitted fields fall back to server defaults.
- `fanout` 1–5, `depth` 1–3. `keepTopK` is sent by the client but has **no UI control** and
  no documented range: `/api/search-lab/config` publishes limits for `fanout` and `depth`
  only, and the client never adjusts `keepTopK` from its default of 5.
- `effort`: the server default is `"instant"`. The names `{"instant","fast","deep"}` appear
  in the (unrelated) `gateway.tako_search` schema's `effort` enum, which is suggestive but
  **not proof** that search-lab accepts the same values — the `/search` UI exposes no
  effort control, so this field is not exercised by the first-party client. Treat `instant`
  as verified and the other two as unverified.
- The body is exactly four keys. No `messages`, no `model`, no `system`.

**Response — `application/x-ndjson; charset=utf-8`, `cache-control: no-store, no-transform`,
`x-accel-buffering: no`.** One JSON object per line. The client reads it with
`readNdjson` (module `10271`), which splits on `\n`, `JSON.parse`es each non-empty line, and
**silently skips** unparseable lines.

**Event types** — the client's reducer in `3r_9svhrldbt0.js` is the complete vocabulary
(`switch (t.t)`); anything not listed is ignored:

| `t` | Fields (as used by the client) | Meaning |
|---|---|---|
| `run_start` | `config`, `ms` | run accepted |
| `call_start` | `id`, `kind` (`plan`\|`retrieve`\|`rerank`\|`followup`\|`synthesize`), `label`, `round`, `llm`, `ms` | a pipeline stage begins |
| `call_end` | `id`, `ms`, `outputTokens`, `inputTokens`, `answerTokens`, `failed?`, `reason?` | stage ends; non-LLM stages send `outputTokens:null`, `inputTokens:0` |
| `queries` | `queries: string[]` | the plan (or follow-up) stage's rewritten queries |
| `round_start` | `round`, `of`, `queries`, `ms` | a retrieval round begins |
| `candidates` | `count`, `fresh?`, `round` | documents retrieved before rerank |
| `docs` | `retrieved`, `kept`, `sources: [{title, url, score}]` | **ranked, deduped sources** |
| `rounds` | `run`, `of`, `stoppedEarly` | round accounting |
| `answer_first_token` | `ms` | synthesis TTFT |
| `answer_delta` | `d: string` | incremental synthesized answer |
| `run_done` | `ms`, `answer` | terminal success; full answer repeated |
| `error` | `message` | terminal failure |

**Live captured stream (probe 2), fanout 1 / depth 1, 200 OK, 3.13 s, 3,890 bytes,
`x-matched-path: /api/search-lab/run`:**

```jsonl
{"t":"run_start","config":{"fanout":1,"depth":1,"keepTopK":3,"effort":"instant"},"ms":0}
{"t":"call_start","id":"c62","kind":"plan","label":"Plan 1 query","round":1,"llm":true,"ms":0.629}
{"t":"call_end","id":"c62","ms":179.96,"outputTokens":15,"inputTokens":547,"answerTokens":15}
{"t":"queries","queries":["four-day workweek trial results countries"]}
{"t":"round_start","round":1,"of":1,"queries":["four-day workweek trial results countries"],"ms":180.07}
{"t":"call_start","id":"c63","kind":"retrieve","label":"Retrieve","round":1,"llm":false,"ms":180.08}
{"t":"call_end","id":"c63","ms":2175.41,"outputTokens":null,"inputTokens":0,"answerTokens":0}
{"t":"candidates","count":4,"fresh":4,"round":1}
{"t":"call_start","id":"c64","kind":"rerank","label":"Rank 4 documents","round":1,"llm":true,"ms":2175.53}
{"t":"call_end","id":"c64","ms":2402.69,"outputTokens":63,"inputTokens":883,"answerTokens":63}
{"t":"docs","retrieved":4,"kept":3,"sources":[
   {"title":"\n\tFour-day work week\n        \n     – Parliament of Australia\n",
    "url":"https://www.aph.gov.au/About_Parliament/Parliamentary_departments/Parliamentary_Library/Research/Issues_and_Insights/2026/FourDayWorkWeek",
    "score":0.6},
   {"title":"4 Day Week Research Reports — 4 Day Week Global",
    "url":"https://www.4dayweek.com/research","score":0.6},
   {"title":"UK four-day week trial hailed as a ‘major breakthrough’ as 92% of companies keep the shorter week | Euronews",
    "url":"https://www.euronews.com/next/2023/02/21/uk-four-day-week-trial-hailed-as-a-major-breakthrough-as-92-of-companies-keep-the-shorter-",
    "score":0.6}]}
{"t":"rounds","run":1,"of":1,"stoppedEarly":false}
{"t":"call_start","id":"c65","kind":"synthesize","label":"Synthesise answer","round":1,"llm":true,"ms":2402.81}
{"t":"answer_first_token","ms":2790.19}
{"t":"answer_delta","d":"The sources describe trials in the UK, Sweden, and Norway, … [1][2][3]\n\n**UK**…"}
{"t":"answer_delta","d":" absenteeism due to sick leave. [2]…"}
{"t":"call_end","id":"c65","ms":2923.39,"outputTokens":203,"inputTokens":1125,"answerTokens":203}
{"t":"run_done","ms":2923.47,"answer":"The sources describe trials … [1]"}
```

Observations that matter to a client:

- **The answer cites by index**: `[1][2][3]` map to `sources[0..2]` in `docs` order. There
  is no per-sentence citation object — just bracket markers in the text.
- **Source titles are raw HTML text**, not cleaned (`"\n\tFour-day work week\n \n – Parliament of Australia\n"`).
  A client must trim/collapse whitespace.
- **Scores are not discriminative in this sample** (all `0.6`); do not rely on them for
  ranking decisions — the server already returns them in rank order.
- **Durations are float milliseconds since run start**, not tokens — useful for latency
  instrumentation.
- **Token counts are real** for LLM stages (`inputTokens`/`answerTokens`/`outputTokens`).
  This is the one place the upstream publishes token usage, unlike `/api/chat`.
- **Terminal semantics:** `run_done` on success, `error` on failure. The probe never saw
  a stream that ended without one of the two, but the client also aborts on Stop, so
  "ends early" is a legitimate client-side outcome.
- The client's `fetchPlaygroundConfig` **retries 401 six times at 300 ms** — a hint that
  the route can answer 401 when a session is stale, even though this probe's request
  (no token) succeeded. Treat `x-session-token` as *recommended*, not proven-required.

### 4.4 The undocumented `POST /api/search`

Probe 3 (negative control) returned **200** with a *valid route* (`x-matched-path:
/api/search`), not a 404:

```
POST /api/search
Content-Type: application/json
(no session token)

{"query":"test"}
→ 200, 570 ms, application/json
{"results":[],"skipped":true}
```

Interpretation:

- The route **exists** and is deployed. `skipped:true` with an empty `results` array is the
  shape of a deliberate no-op — the most plausible reading is that `/api/search` is the
  server-side search tool used by the `/api/chat` web-search agent, and it requires an
  internal contract (a different body shape, a header, or an internal invocation context)
  that a bare `{"query": …}` does not satisfy.
- **No client bundle references it** — not the chat chunk, not the search-lab chunk, not
  the voice chunk. If it is used by `/api/chat`, the call is entirely server-side.
- **Do not build against it.** One probe with one body shape is not a specification; the
  response gives no schema for a successful call, and the route is undocumented.

---

## 5. Live verification log

Environment: this host is Vercel-challenged for all plain HTTP clients (see §1.2). Probes
ran as in-page `fetch()` from a challenge-cleared Chromium page at
`https://chat.inceptionlabs.ai/search`.

| # | Time (UTC) | Request | Status | Latency | Result |
|---|---|---|---|---|---|
| 1 | 05:24:58 | `GET /api/search-lab/config` | 200 | 196 ms | config JSON (§4.2) |
| 2 | 05:25:19 | `POST /api/search-lab/run` (fanout 1, depth 1, keepTopK 3, instant) | 200 | 3,126 ms | 15 NDJSON lines, `run_done` (§4.3) |
| 3 | 05:25:44 | `POST /api/search` `{"query":"test"}` | 200 | 570 ms | `{"results":[],"skipped":true}` |

Headers observed on probe 2 (verbatim, minus `date`/`x-vercel-id`):

```
cache-control: no-store, no-transform
content-encoding: br
content-type: application/x-ndjson; charset=utf-8
permissions-policy: camera=(), geolocation=(), microphone=(self)
referrer-policy: strict-origin-when-cross-origin
server: Vercel
strict-transport-security: max-age=31536000; includeSubDomains
x-accel-buffering: no
x-content-type-options: nosniff
x-matched-path: /api/search-lab/run
x-vercel-cache: MISS
```

Notes:

- `content-encoding: br` — the NDJSON stream is **Brotli-compressed end-to-end**. This is
  only an integration trap if the client advertises `br`: Go's `net/http` does **not**
  decompress Brotli, and it will pass the compressed bytes through untouched. A Go client
  must therefore either (a) not send `br` in `Accept-Encoding` (send `identity`) and read
  plain NDJSON, or (b) wire in an `andybalholm/brotli`-style reader. The browser probe
  decoded it automatically, which is why the captured lines are already plaintext.
- `x-accel-buffering: no` — the server explicitly disables proxy buffering, i.e. this is
  meant to be consumed incrementally.
- `cache-control: no-store` — no caching, as expected for a POST pipeline.
- No rate-limit headers were returned on any probe; the client nevertheless implements a
  429 retry loop for `/api/chat`, which suggests 429s do occur under load.

**Probe budget respected:** 3 probes, 21 s apart, sequential, single session. No chat
requests were issued.

---

## 6. What a consumer should do with this

### 6.1 If the goal is citations on a chat completion

Keep using `POST /api/chat` with `webSearchEnabled:true`. Map `source-url` events to
`message.annotations[]` / `delta.annotations[]` with `type:"url_citation"`, **dropping
`__searching__` and `__search_error__`**, and attach them to the assistant message whose
stream produced them (the client keys them by message id). There is no way to request a
search, no way to supply the query, and no way to know in advance whether a search will
happen — only that it is enabled.

### 6.2 If the goal is "give me search results for a query"

`POST /api/search-lab/run` is the only documented-by-observation way to get sources for a
caller-supplied query. Practical contract:

1. Send `{query, config:{fanout,depth,keepTopK,effort}, timezone, language}`.
2. Read NDJSON; stop at the `docs` line if only sources are wanted.
3. Expect `sources: [{title,url,score}]` in rank order, plus `queries` (the rewritten
   queries) and `candidates.count` for diagnostics.
4. If the answer is wanted too, keep reading to `answer_delta` / `run_done`.
5. Handle `error {message}` as terminal, and `x-session-token` 401 as "refresh session and
   retry" (the first-party client retries 6× at 300 ms).

Caveats: it is a **synthesis pipeline**, not a pure search API; latency is seconds (3.1 s
for the smallest possible config); cost is several LLM calls per run (plan + rerank +
synthesise, plus follow-up per extra round); `retrievalAvailable` can be `false`, in which
case the route should not be used at all.

### 6.3 What *not* to conclude

- The four `gateway.*_search` schemas in the bundle are **not** this product's search. They
  are AI Gateway provider definitions shipped inside the SDK. They are registered
  (`exaSearch`, `parallelSearch`, `perplexitySearch`, `takoSearch`) but never referenced by
  application code, never sent as `tools`, and never appear in a request body. Grep for
  `exaSearch(` / `_N.` / `hZ.` in the archived build returns only the registration site.
- The absence of `/api/search` from the bundles does **not** mean it is unused — it means
  any call to it is server-side.
- A `source-url` with `title:"__search_error__"` is not a citation; it is a failure marker.

---

## 7. Evidence index

| Claim | File · symbol / location |
|---|---|
| Chat transport is `/api/chat` only | `2wmuhr7tyn5ak.js` · `new v5({api:"/api/chat", headers:()=>br.sessionHeaders(), fetch: …})` (archived: `1ye2f2fzco-j2.js` · `new xs({api:"/api/chat", …})`) |
| Chat body fields | `2wmuhr7tyn5ak.js` · `sendMessage({text}, {body:{reasoningEffort, webSearchEnabled, voiceMode, timezone, language}})` |
| Web search default `true`, reasoning default `"medium"` | `2wmuhr7tyn5ak.js` · chat state hook `useState("medium")` / `useState(!0)` |
| 429 retry (2×, 1.5 s×n) + `user-agent` deletion | `2wmuhr7tyn5ak.js` · transport `fetch` option |
| `__searching__` sentinel | `2wmuhr7tyn5ak.js` · `k?.parts.some(p=>"__searching__"===p.title)`; `isSearching = !!I && !S && j` |
| `__search_error__` sentinel | `2wmuhr7tyn5ak.js` · `searchErrorSet` memo (`"__search_error__"===e.title`) |
| `searchResultsMap` keyed by message id, sentinel filtered | `2wmuhr7tyn5ak.js` · `useMemo(()=>{ … e[t.id]=r.map(…) })` |
| Web search toggle (ARIA) | `2wmuhr7tyn5ak.js` · `function xL/{…} aria-label:"Web search on — click to disable"` (archived: `xL` in `1ye2f2fzco-j2.js`) |
| `/search` nav entry + blurb | `2wmuhr7tyn5ak.js` · nav array `{href:"/search",label:"Search",blurb:"Watch a retrieval pipeline run"}`; also `2969v_5aik-bo.js` |
| Search-lab page + stage map | `3r_9svhrldbt0.js` · `SearchLab` component; `plan/retrieve/rerank/followup/synthesize` map; defaults `p={fanout:4,depth:2,keepTopK:5,effort:"instant"}` |
| `/api/search-lab/config` call + 401 retry | `3r_9svhrldbt0.js` · `fetchPlaygroundConfig("/api/search-lab/config")` → module `60525` `async function c(e,t=0)` |
| `/api/search-lab/run` request | `3r_9svhrldbt0.js` · `fetch("/api/search-lab/run",{… body:JSON.stringify({query:r,config:n,timezone,language})})` |
| NDJSON reader | `3r_9svhrldbt0.js` · module `10271` `readNdjson` (splits `\n`, skips parse errors) |
| NDJSON event vocabulary | `3r_9svhrldbt0.js` · reducer `switch(t.t)` — `call_start`, `round_start`, `rounds`, `call_end`, `queries`, `candidates`, `docs`, `answer_first_token`, `answer_delta`, `run_done`, `error`; plus `run_start` from the probe |
| `/api/search` route (probe only) | Live probe 3; no bundle reference |
| `/api/session` + 13-min refresh + `sessionHeaders` | `1bphncyfur5lp.js` · `SessionProvider`, `setInterval(l,78e4)`, `sessionHeaders` |
| `/api/follow-ups` shape | `2wmuhr7tyn5ak.js` · `fetch("/api/follow-ups", … body:JSON.stringify({messages:r}))` |
| `/api/voice/*`, `/api/stt`, `/api/tts` | `0zw1816s_o6fw.js`, `2wmuhr7tyn5ak.js` |
| AI SDK version | `2wmuhr7tyn5ak.js` · literal `"4.0.51"` (archived: `"4.0.50"`, `"4.0.49"`) |
| `gateway.*_search` = SDK catalogue, unused | `2wmuhr7tyn5ak.js` · `fV({id:"gateway.exa_search", inputSchema: …})` etc., registered as `{exaSearch, parallelSearch, perplexitySearch, takoSearch}`; no application call site |
| Audit's captured chat-with-search stream | `/tmp/audit-inception.md` §1.1–1.2 (`reasoning-0 → text-0 → 13× source-url → reasoning-1 → text-1 → [DONE]`); raw `out_web.json` scratch file no longer present |
