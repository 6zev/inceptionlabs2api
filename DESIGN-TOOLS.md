# DESIGN — Tool calling over a prompt, with on-demand injection

Status: design of record for v8. Written before the code; `docs/SPEC.md` §4.1 is
the normative summary and this document is the evidence trail.
Companion evidence: `/root/inception-dig/TOOLCALL-PATH.md` (upstream has **no**
native client-declared tool path).

Every decision below is tagged **[D]** (documented by the cited source),
**[M]** (measured — cited study or a probe we ran), or **[I]** (inferred by us
from the documented mechanism; flagged so it can be falsified).

---

## 0. Problem statement

The upstream web endpoint `/api/chat` accepts exactly
`{messages, reasoningEffort, webSearchEnabled, voiceMode, timezone, language, id,
trigger}` and returns AI-SDK text/reasoning/source parts. Ten probes proved it
has no `tools` slot and never emits `tool-input-*` events, even though the same
deployment runs server-owned tools in code-lab and voice **[M:
TOOLCALL-PATH.md §0, §2]**. Tool calling therefore has to be built here:

```
OpenAI client ── tools/tool_choice ──▶ gateway ── prompt ──▶ /api/chat
OpenAI client ◀── tool_calls ◀───────── parser  ◀── text  ── /api/chat
OpenAI client ── role:"tool" ─────────▶ gateway ── replay ─▶ /api/chat
```

Two facts from the dig drive the wire-format decision:

1. Mercury **reproduces an instructed XML tool format verbatim**. In tc3 it
   answered plain text containing
   `<tool_calls><invoke name="get_weather"><parameter name="location">Paris</parameter></invoke></tool_calls>`,
   and in tc4 `<web_search query="today's date in Tokyo" />` **[M:
   TOOLCALL-PATH.md §2.2, probe/outTC/tc3-tools-aisdk-top/body.bin]**.
2. In tc2 it emitted `{"city": "Paris"}` as plain text when told to use a
   `get_weather` tool **[M: probe/outTC/tc2-tools-openai/body.bin]**. So JSON
   payloads are equally natural for this model.

And one fact about its streaming shape: every probe delivered the **whole
answer in a single `text-delta`** (tc2, tc3, tc4 bodies are one delta each;
the live baseline captured 2026-10-21 is two deltas for a four-word answer)
**[M]**. Buffering a tool block until it is complete therefore costs
approximately nothing on this upstream — which matters for §3.

---

## 1. Wire format

**Decision: a system-message preamble containing a `<tools>` list (one compact
JSON object per line) and a strict `<tool_call>{"name":…,"arguments":{…}}</tool_call>`
output contract.**

```
# Tool calling

You can call the functions listed below. To call a function, emit exactly:

<tool_call>
{"name": "get_weather", "arguments": {"city": "Paris"}}
</tool_call>

Emit one block per call. Do not wrap a block in a code fence. Only use the
function names below; never invent one. If no function is needed, answer
normally.

<tools>
{"name":"get_weather","description":"…","parameters":{…}}
</tools>
```

### Why this format

| Candidate | Evidence | Verdict |
|---|---|---|
| **`<tool_call>{json}</tool_call>`** (Hermes/Qwen family) | Qwen2.5/3 chat templates teach exactly this and replay it identically **[D: HuggingFace `Qwen/Qwen2.5-7B-Instruct` rev `a09a3545`, `tokenizer_config.json` chat_template; `Qwen/Qwen3-8B` rev `b968826d`]**. vLLM ships a production streaming parser for it **[D: `vllm/tool_parsers/hermes_tool_parser.py` @ `a0a645a`, `_extract_content` L122–139, `_extract_tool_call_jsons` L141–165]**. Mercury emits JSON args naturally **[M: tc2]**. | **Chosen.** One unambiguous JSON object; argument *types* survive (numbers, bools, nested objects) with no per-parameter coercion; parser is the simplest safe one. |
| `<tool_calls><invoke name><parameter name>` (Anthropic-ish) | **Proven reproduced verbatim by this model** **[M: tc3]**. But every production parser of it needs a schema-aware type converter (SGLang `_convert_param_value`, vLLM `arg_value_converter`) and streaming parameter values means JSON-escaping each fragment **[D: `vllm/parser/minimax_m2.py` @ `a0a645a`, `extend()`]**. | **Rejected as the emitted format, accepted by the parser as drift tolerance** (§2). tc3 is why: the model may fall back to it, and a call in the proven format must not be lost. |
| Nested XML parameter format with typed values (Qwen3-Coder) | High token cost, `tojson` per non-string value, replay ambiguity **[D: Qwen3-Coder template rev `b2cff646`]** | Rejected. |
| Flattened bullet list (`name (type, required): desc`) | aurora's `compactToolsPrompt` drops nested objects, arrays, `anyOf`, `additionalProperties` **[D: `aurora-develop/aurora` @ `cbfbabc`, `internal/toolcall/prompt.go` L69–116]** | Rejected: silently loses schema. |
| TSV / YAML "denser" encodings | No published accuracy measurement; the MCP-metadata study shows parameter schemas materially shift tool selection, so size alone is not evidence **[D: arXiv 2610.14334]** | Rejected without an A/B. |
| One JSON array of calls (DeepSeek `[TOOL_CALLS][…]`) | Parser has to fuzzy-match whitespace-variant markers **[D: `NIyueeE/ds-free-api` @ `99f7730`, `tool_parser.rs` L301–384]** | Rejected: more drift surface, no benefit. |

### Serialization details

- Tools are rendered **compact** (`{"name":…,"description":…,"parameters":…}`,
  no `type:"function"` wrapper, no indentation). Measured on a representative
  10-tool set: pretty JSON 8 522 B → compact 5 861 B (**−31 %**), and a
  name+one-line index 2 501 B **[M: local measurement 2026-10-21, 10 tools with
  description + object schema; see §6]**. Pretty-printing buys nothing from a
  model; it is pure context cost.
- An unparseable or missing `parameters` is rendered as `{}` rather than
  dropped; the name and description are what the model selects on.
- The preamble is **one system message placed first**, matching the Qwen/Hermes
  templates (tools in the system turn) **[D: Qwen template; Hermes-3 template
  rev `896ea440`]**. It is emitted only when `len(tools) > 0` (§6).

---

## 2. Response parsing

**Decision: a single-pass scanner over accumulated text with a tag-prefix
holdback; a block becomes a call only if the JSON parses *and* the name is one
of the declared tools. Anything else is emitted as ordinary content.**

### Accepted forms

1. **Primary**: `<tool_call>` … `</tool_call>` where the body is a JSON object
   with `name` (string) and `arguments` (object). An optional `id` is ignored
   (we mint our own, §4).
2. **Drift**: `<tool_calls>` … `</tool_calls>` containing one or more
   `<invoke name="X">` … `</invoke>` blocks whose `<parameter name="P">value</parameter>`
   children become string-valued arguments. This is the exact form tc3 proved
   the model reproduces **[M: TOOLCALL-PATH.md §2.2]**. Values are kept
   verbatim, including whitespace and escapes; they are **not** coerced to
   numbers or booleans — the model saw strings, and inventing types would be
   second-guessing.

### Parser rules

| Rule | Rationale / citation |
|---|---|
| Hold back only the longest suffix of unemitted text that is a prefix of a start marker (`partial_tag_overlap`). Everything before it is content. | vLLM's canonical rule, and the only way a marker split across chunks is never leaked **[D: `vllm/tool_parsers/utils.py` @ `a0a645a`, `partial_tag_overlap` L75]**. |
| Inside a block, buffer until the matching close marker. | §3 explains why whole-block buffering is the correct choice under the pass-through guarantee. |
| Closed block that does not parse (JSON error, missing/non-string `name`, arguments not an object) → emit the **raw block text** as content. | Pass-through philosophy: malformed model output is prose, never a gateway error. Matches GLM-Free-API's "closed but unparseable → visible text" **[D: `izaart95-jpg/GLM-Free-API` @ `a958fc1`, `internal/zbridge/agent.go` L1367–1397]** and llama.cpp's demotion to content-only **[D: `ggml-org/llama.cpp` legacy `common/chat.cpp` @ `34df42f`]**. |
| Block still open at end of stream → emit everything buffered (marker included) as content, `finish_reason:"stop"`. | Never invent a truncated call. Contrast: GLM ships unbalanced arguments as a valid call **[D: `agent.go` L1517–1524]**, ds-free leaks the block as prose past 64 KiB **[D: `tool_parser.rs` L640–649]**. Both are worse than honest text. |
| Name not declared in the request → emit the block as text. | Stops a user asking "show me `<tool_call>{\"name\":\"rm\"}…`" from executing anything, and stops hallucinated names. vLLM's engine parser and LocalAI both validate names **[D: `vllm/parser/...` `validate_tool_names=True`; `mudler/LocalAI` @ `7f7b10f`, `pkg/functions/parse.go`]**; ollama errors outright **[D: `ollama/ollama` @ `2cd63f7`, `model/parsers/glimmer.go`]**. |
| A block wrapped in a Markdown code fence is still parsed (the fence is outside the markers). | Fences are a common model habit; the declared-name check keeps this safe. vLLM non-streaming strips fences before parsing **[D: `hermes_tool_parser.py`]**; we simply ignore them as surrounding text. |
| Multiple blocks → multiple calls, `index` 0..n−1 in appearance order. | Hermes/Qwen allow sequential calls; the OpenAI chunk schema keys parallel calls by `index` **[D: openapi.yaml L42245–42273]**. |
| Text before, between, and after calls is content. | OpenAI's own recorded streams send content and tool calls in the same response **[D: real capture `openai-python/.inline-snapshot/external/c6aa7e39….bin`; vLLM test asserts content + call in one chunk]**. |
| Hard cap 64 KiB per block (whole response cap for the non-streaming scan). A block exceeding it is emitted as content and scanning resumes after it. | Memory bound on a hostile upstream; precedent: ds-free `MAX_XML_BUF_LEN = 64 * 1024` **[D: `tool_parser.rs` L26]**. |
| Cuts are rune-safe: a holdback boundary never splits a UTF-8 sequence. | GLM's `runeSafeCut` exists for exactly this **[D: `agent.go` L1235–1246]**; our `text-delta` chunks are byte strings from JSON decoding, so a split rune is representable. |

### Explicit non-goal

We do **not** attempt to repair JSON with a second model call (ds-free does,
discarding the stream and issuing a repair request **[D: `response.rs`
L167–258]**). That turns a formatting accident into extra latency, an extra
upstream request, and a possible hard error. Pass-through says: show the text.

---

## 3. Streaming

**Decision: release content up to the marker-prefix holdback immediately; emit
a completed call as one `tool_calls` delta; terminate with
`finish_reason:"tool_calls"`.**

State machine:

```
CONTENT  --start marker found-->  BLOCK (buffer everything)
BLOCK    --close marker, valid JSON, declared name-->  emit call, -> CONTENT
BLOCK    --close marker, invalid-->                    emit raw block as content
BLOCK    --end of stream-->                            emit raw buffer as content
```

### Why whole-block buffering, not incremental argument deltas

The alternative — streaming `arguments` fragments as they arrive — is what
vLLM/GLM do, and it is genuinely lower latency **[D: `GLM-Free-API agent.go`
L1360–1547; vLLM main Hermes re-parse+diff]**. But it is fundamentally
**incompatible with "malformed ⇒ plain text"**: once a tool-call header with
`id` and `name` has been sent, the gateway cannot retract it when the block
turns out to be unparseable. GLM resolves this by shipping the broken call
anyway **[D: `agent.go` L1517–1524]**; ds-free by leaking prose **[D]**. We
choose the guarantee over the latency, and the upstream makes the latency cost
≈0: Mercury sends whole answers in single `text-delta` events **[M: tc2–tc4,
live baseline]**. Two other production shims make the same call — ds-free
buffers between the tags, and qwen-free-api's default "modern" interceptor
emits the complete call at the close tag **[D: `ds-free-api` `ToolParseState`;
`Godde3s/qwen-free-api` @ `71c2e65`, `internal/qbridge/agent.go` L1006–1030]**.

We do **not** send empty-delta keepalives while buffering: ds-free added them
and its issue #87 documents that clients accumulated phantom tool calls from
empty `tool_calls` frames **[D: `ds-free-api` `tool_parser.rs` L515–543]**.

### Frame shape (streaming)

```
1. role frame:        {"delta":{"role":"assistant"}, finish_reason:null}
2. content frames:    one per released content run (preamble text, inter-call text)
3. call frame:        {"delta":{"tool_calls":[{"index":i,"id":"call_…",
                        "type":"function","function":{"name":N,"arguments":"<complete JSON>"}}]}}
4. finish frame:      {"delta":{…annotations…}, finish_reason:"tool_calls"}
5. data: [DONE]
```

`index` is the only schema-required field on a tool-call chunk; `id`, `type`
and `function.name` are sent on the first (here: only) chunk of each call, as
OpenAI does **[D: openapi.yaml L42245–42273; Function calling guide "many of
these fields are only set for the first delta"; real capture
`c6aa7e39….bin`]**. `arguments` is a JSON-encoded **string**, not an object
**[D: openapi.yaml L42265–42271]**. `finish_reason:"tool_calls"` is emitted iff
at least one call was parsed; otherwise `"stop"` as today.

### Frame shape (non-streaming)

`message.content` keeps its existing string type (`""` when the model only
called a tool — OpenAI emits `null`, and clients treat both as empty
**[D: openapi.yaml `ChatCompletionResponseMessage`, `content` anyOf string|null]**);
`message.tool_calls[]` carries `{id,type:"function",function:{name,arguments}}`;
`finish_reason:"tool_calls"`. `annotations` continues to ride the same message.

### Failure mid-stream after a call was emitted

The existing contract is unchanged: one `{"error":…}` SSE frame then
`data: [DONE]`, with no finish frame. A client that already received a call
sees an aborted stream, which is exactly what OpenAI does on a dropped stream.

---

## 4. Stateless round trips (`role:"tool"` replay)

The upstream is stateless; the client replays full history. Two messages need
tool-aware encoding:

**Assistant turn with `tool_calls`** — its text content, then one block per call:

```
<tool_call>{"id":"call_…","name":"get_weather","arguments":{"city":"Paris"}}</tool_call>
```

`arguments` is embedded **verbatim as JSON** when it parses; when it does not
(clients may echo anything), it is embedded as a JSON string, so the replayed
block is always syntactically valid. The `id` is included **only in replay** so
the model can associate the result with the call; the model is not asked to
emit ids and any `id` it does emit is ignored by the parser. (GLM's replay
omits ids while its results carry `call_id`, leaving the attribute dangling —
the bug this avoids **[D: `GLM-Free-API agent.go` L392–401, L481–503]**.)

**Tool result** — rendered as a user turn, wrapped in a tagged envelope, with
consecutive results grouped into one user message:

```
<tool_response call_id="call_…" name="get_weather">
{"tempC":21}
</tool_response>
```

Evidence: Qwen2.5/3 templates put tool results in a **user** turn as
`<tool_response>…</tool_response>` and group consecutive tool messages into one
turn **[D: Qwen2.5 template]**. Hermes uses a `tool` role with the same
`<tool_response>` envelope **[D: Hermes-3 template]**. The upstream only accepts
`system|assistant|user` **[D: SPEC §4]**, so the user-turn variant is the only
available one. `tool_call_id` is preserved in the envelope; it is never
dropped. The `name` attribute is added when the matching assistant `tool_calls`
entry is present in the same request (lookup by id), because the OpenAI
`role:"tool"` message itself has no `name` field **[D: openapi.yaml
`ChatCompletionRequestToolMessage`, required `[role, content, tool_call_id]`]**.

**Gating (zero regression).** All tool-aware encoding is gated on
`len(request.tools) > 0`. A request without a non-empty `tools` array — even if
it carries `role:"tool"` history — takes today's exact code path: `tool` role
coerced to `user` with its bare content, assistant `tool_calls` dropped, no
preamble, no parser. This is what makes the no-tools byte-identical guarantee
testable (§7, T1).

---

## 5. `tool_choice` semantics

OpenAI's spec text, verbatim from `openapi.yaml` @ `3db76f5` (L43190–43222):

> `none` means the model will not call any tool and instead generates a message.
> `auto` means the model can pick between generating a message or calling one or
> more tools. `required` means the model must call one or more tools. Specifying
> a particular tool via `{"type": "function", "function": {"name": "my_function"}}`
> forces the model to call that tool.
> `none` is the default when no tools are present. `auto` is the default if
> tools are present.

The named variant is a first-class Chat Completions shape with a **nested**
`function` object (`required: [type, function]`) **[D: openapi.yaml
`ChatCompletionNamedToolChoice` L42375–42397]**. The function-calling guide's
flat `{"type":"function","name":…}` is the *Responses* shape and is not valid
here; we implement the schema, not the guide snippet.

| `tool_choice` | Injection | Prompt instruction | Parsing |
|---|---|---|---|
| absent | all tools | none | on |
| `"auto"` | all tools | none (prose allowed) | on |
| `"none"` | **none** | none | **off** — request takes the legacy path (spec: "imitate the behavior of passing no functions" **[D: Function calling guide]**) |
| `"required"` | all tools | "You MUST call at least one function before answering." | on |
| `{"type":"function","function":{"name":"X"}}` | **only X** | "You must call the function `X`." | on, but X is the only declared name, so any other name is prose |

Restricting the manifest to the named tool is a faithful emulation *and* saves
context: the spec says the model is forced to call that tool, so the other
schemas cannot affect the outcome. ds-free is the only surveyed project that
implements all four variants; its named/required instructions are the model we
follow **[D: `ds-free-api` `request/tools.rs` L29–99]**. aurora maps
`required`→`auto` and GLM/qwen-free-api never read the field at all **[D:
`aurora` `prompt.go` L48–52; GLM `handlers.go` L32 + no other reference]**.

`parallel_tool_calls:false` (default true per spec) adds "Call at most one
function per turn." — the guide's wording is "ensures exactly zero or one tool
is called" **[D: Function calling guide]**, so under `required` the instruction
becomes "exactly one".

**If the model ignores `required`** and answers in prose, the gateway returns
the prose with `finish_reason:"stop"`. It does not fabricate a call and does
not error: the request was answerable, the model simply declined, and the
client can see that honestly. (Same reasoning as pass-through.)

---

## 6. On-demand injection (the context-pressure requirement)

### 6.1 Baseline — zero cost without tools

The preamble, the manifest, the parser and the replay encoder are created only
when `len(tools) > 0`. A tool-less request builds exactly the same
`upstream.ChatRequest` as today, byte for byte, and its response frames are
produced by the same code path. This is not just a design claim: it is pinned
by a golden test and by a live request compared against a pre-change capture
(§7, T1 / V1).

Cost when tools *are* present:

- **Compact JSON, no indentation** — measured −31 % bytes vs pretty on 10
  representative tools **[M]** (a tiktoken run over the same shapes measured
  1 589 → 839 tokens, −47 % **[M: research measurement, `cl100k_base`]**).
- **Only the declared tool under a named `tool_choice`** (§5) — can remove the
  entire rest of the manifest.

### 6.2 Tiered manifest above 30 tools

**Decision: when a request declares more than 30 tools, inject a tier-1 index
(names + descriptions, no parameter schemas) plus a synthetic
`tool_schema` loader. If the model calls the loader, the gateway expands those
schemas and re-asks the upstream internally, at most twice.**

Evidence and why this shape:

- Anthropic's docs report that selection degrades once **30–50 tools** are
  available and that tool search reduces a 58-tool / ~55 K-token setup by over
  85 % by loading only 3–5 tools **[D: platform.claude.com "Tool search tool"]**.
  30 is the lower bound of that published range — the threshold is cited, not
  invented.
- OpenAI's `defer_loading` keeps the **name and description** in context and
  defers the parameter schema: "in practice tool search is mostly deferring the
  parameter schema" **[D: developers.openai.com "Tool search"]**. Our tier 1 is
  exactly that: the same names and descriptions, schema removed.
- The loader is **model-driven, not a fixed top-k**. A fixed shortlist of 5
  finds nothing when the right tool ranks 6th–20th; an adaptive depth reaches
  90.3 % vs 90.8 % coverage while loading 7 tools on average **[D: arXiv
  2605.24660]**. A model that can ask for *any* declared name by name avoids
  the fixed-k failure; Claude Code's `select:<name>` escape hatch is the same
  idea **[D: code.claude.com MCP tool search]**.
- The expansion is a **server-internal extra round**, mirroring Anthropic's
  documented "adds a search step before tool invocation" **[D]**. It happens
  before anything is written to the client (content and calls are buffered
  during the probe round), so the client sees one normal response.
- Bounded at **2 rounds**. This is not a client-loop guard (the OpenAI protocol
  is client-driven and each of our requests is stateless, so no guard is
  needed there — verified against the protocol shape in §4); it bounds *our*
  internal re-ask loop, which otherwise could repeat at gateway expense.

Cost: a tiered request that uses the loader pays one extra upstream generation.
A tiered request that does not use it pays only the generation it would have
paid anyway, plus the buffering described in §3 (≈0 on this upstream). If the
second expansion still returns a loader call, that block is demoted to text and
the response ends with `finish_reason:"stop"` — the client never sees the
synthetic tool.

### 6.3 Techniques considered and rejected

| Technique | Why rejected |
|---|---|
| **Blind two-pass**: answer first with no tools, re-ask with schemas only if the model says it needs one | The model cannot ask for a tool it has never seen; the signal is unreliable, and every tool-using request pays a full extra generation **[I]**. Anthropic/OpenAI both keep an index visible rather than nothing **[D]**. |
| **Lexical/embedding RAG shortlist** | Retrieval is the weak link even with trained retrievers — off-the-shelf IR models perform poorly on tool retrieval and the loss propagates to task success **[D: arXiv 2503.01763 ToolRet]**. A stdlib-only lexical ranker has no evidence at all **[I]**. |
| **Fixed top-k=5 shortlist** | Measured to miss correct tools ranked 6th–20th **[D: arXiv 2605.24660]**. |
| **TSV/YAML manifests** | ~2× smaller in one measurement, but no accuracy A/B exists, and schema wording itself shifts tool selection **[D: arXiv 2610.14334]**; a size win that changes selection is not a win **[I]**. |
| **Per-session manifest caching** | There is no session: every request is stateless and the upstream has no documented prompt cache **[D: SPEC §2; TOOLCALL-PATH §1.4]**. Nothing to cache across requests; within one request the manifest is built once. |
| **Third-party toolkit / XYML** | Excluded by the task. Not needed: the format above is 30 lines of prompt and 300 lines of parser. |

---

## 7. Failure modes and test plan

Every row is a test in `app/internal/tools` (unit) and/or
`app/internal/server` (HTTP-level). `T1` is the regression gate.

| # | Failure / case | Required behavior | Test |
|---|---|---|---|
| T1 | Request has **no `tools`** (with/without tool-role history) | Byte-identical upstream body and SSE frames to the pre-change gateway; no preamble, no parsing | Unit: `NewChatRequest` equality; integration: golden prompt + golden frame sequence; live: V1 vs pre-change capture |
| T2 | Model ignores the format entirely | All text as `content`, `finish_reason:"stop"`, no `tool_calls` | Streaming + non-streaming parser fixtures |
| T3 | Model emits half a call (open marker, no close) | Buffered text released verbatim as content at EOS, `finish_reason:"stop"` | Parser flush test; server SSE test |
| T4 | Closed block with invalid JSON | Raw block emitted as content | Parser + SSE |
| T5 | Block names an undeclared function | Raw block emitted as content, no call | Parser |
| T6 | Text **and** a call in one response | Content frames then call frame; `finish_reason:"tool_calls"` | SSE ordering test |
| T7 | Two/three calls | `index` 0,1,2; each with its own id/name/arguments | Parser + SSE |
| T8 | Nested JSON / arrays / unicode / escaped quotes in arguments | Arguments string byte-exact | Parser table (includes `{"a":{"b":[1,{"c":"é"}]}}`, `"é"`, quotes/backslashes) |
| T9 | Marker split across chunks at **every** byte boundary | No marker fragment ever leaks as content; exactly one call | Property-style test: for i in 1..len-1 split the input at i and feed both halves |
| T10 | Multi-byte rune split at a holdback boundary | Content deltas are valid UTF-8 and concatenate to the input | Parser test feeding `[]byte` halves mid-rune |
| T11 | `tool_choice:"required"` but model answers prose | Prose content, `finish_reason:"stop"`; no fabricated call | Server test with scripted upstream |
| T12 | Named `tool_choice` | Manifest contains only that tool; other names in output are prose | Manifest test + parser/server test |
| T13 | `tool_choice:"none"` with tools present | Legacy path, no injection, no parsing | Server test comparing upstream body to no-tools body |
| T14 | Replay of assistant `tool_calls` + 2 consecutive `tool` results | Assistant blocks with ids; one user turn with two `<tool_response call_id=…>` blocks in call order | Round-trip unit test |
| T15 | Tool result whose `tool_call_id` matches nothing | Still rendered (name omitted), never dropped | Round-trip unit test |
| T16 | Duplicate/absent id from client history | Gateway mints a fresh unique id per emitted call | Id test (1000 draws unique, `call_` + 24 alnum) |
| T17 | Block > 64 KiB | Emitted as content, scanner resumes; memory bounded | Parser test |
| T18 | Code-fenced block | Parsed if the name is declared | Parser test |
| T19 | `tools` is present but not an array / function has no name | 400 `invalid_request_error` (structural class 2) — a body the wire cannot express; no upstream call | Decode tests |
| T20 | Tool call plus `source-url` citations | Annotations still ride the finish frame next to `tool_calls` | Server SSE test |
| T21 | >30 tools | Tier-1 manifest has no `parameters`; loader tool present | Manifest unit test |
| T22 | >30 tools, model calls the loader | Internal re-ask includes the requested schemas; client sees only the final answer; ≤2 rounds | Server test with scripted upstream call counter |
| T23 | >30 tools, loader called on both rounds | Loader block demoted to text, `finish_reason:"stop"` | Server test |
| T24 | ≤30 tools | Full schemas, no loader (tiering must not trigger) | Manifest unit test |
| T25 | `parallel_tool_calls:false` | Instruction present; parser still returns whatever the model sent (it does not enforce) | Manifest test |

Coverage target: ≥80 % over `app/internal/tools`, with every row above present
as an executable assertion.

---

## 8. Live validation plan (budget: ≤10 requests, ≥20 s apart)

The gateway runs locally against the real upstream; each numbered item is one
request.

| # | Request | Pass condition |
|---|---|---|
| V1 | **No tools**, streaming, fixed prompt (`Reply with exactly: baseline-ok`), compared against the pre-change capture `testdata/no-tools-baseline.sse` | Same frame sequence with id/created normalised; content bytes identical; `finish_reason:"stop"`; no extra frames |
| V2 | `get_weather` non-streaming, `tool_choice:"auto"` | `finish_reason:"tool_calls"`; declared name; arguments parse; **or** prose (allowed by pass-through — parser failure is not a gateway failure) |
| V3 | Same tool, streaming | A `tool_calls` delta with id/name/complete arguments then `finish_reason:"tool_calls"`; or prose frames ending `stop` |
| V4 | `tool_choice:"required"` | A call, or honest prose with `stop` under the documented no-fabrication rule |
| V5 | Multi-round: call → `role:"tool"` result → final answer | Second response is prose that uses the result; no preamble leakage into content |
| V6–V9 | Reserve: one tiered request (>30 tools) and retries if V2–V5 produce a format-drift case worth observing | Tiering: no parameter schemas in prompt log, real answer returned |

A prose answer from V2–V4 is recorded as a **probe result about the model**, not
treated as a test failure; the parser's own guarantees are pinned by T2–T20.

---

## 9. Sources

Pinned raw sources actually read (all fetched 2026-10-21):

- OpenAI OpenAPI spec `3db76f5c62f01cf8e0c1cc602fdf9546b341961b` —
  https://raw.githubusercontent.com/openai/openai-openapi/3db76f5c62f01cf8e0c1cc602fdf9546b341961b/openapi.yaml
  (§`ChatCompletionToolChoiceOption` L43190, `ChatCompletionNamedToolChoice`
  L42375, `ChatCompletionMessageToolCallChunk` L42245,
  `ChatCompletionRequestToolMessage`, `ChatCompletionResponseMessage`).
- OpenAI Function calling guide —
  https://developers.openai.com/api/docs/guides/function-calling (tool_choice,
  streaming deltas, `parallel_tool_calls`, strict).
- Recorded OpenAI streams from `openai/openai-python`:
  `.inline-snapshot/external/c6aa7e397b7123c3501f25df3a05d4daf7e8ad6d61ffa406ab5361fe36a8d5b1.bin`
  (single call) and `f82268f2fefd5cfbc7eeb59c297688be2f6ca0849a6e4f17851b517310841d9b.bin`
  (parallel calls).
- vLLM `main` `a0a645abdad134bafbc1a8954dd436e4fb0a5e61`:
  `vllm/tool_parsers/hermes_tool_parser.py`, `vllm/tool_parsers/utils.py`,
  `vllm/parser/minimax_m2.py`, `vllm/parser/qwen3.py`,
  `vllm/parser/engine/incremental_lexer.py`,
  `vllm/entrypoints/openai/chat_completion/stream_processor.py`.
- vLLM `v0.10.1` `aab549870df50edf0512f0a59b574f692f546465`:
  `vllm/entrypoints/openai/tool_parsers/{hermes,mistral,llama,pythonic,deepseekv3}_tool_parser.py`.
- Chat templates (HuggingFace raw `tokenizer_config.json`): Qwen2.5-7B rev
  `a09a3545…`, Qwen3-8B rev `b968826d…`, Qwen3-Coder rev `b2cff646…`,
  Hermes-3-Llama-3.1-8B rev `896ea440…`, Mistral-Nemo rev `04d8a905…`,
  Llama-3.1 template `vllm/examples/tool_chat_template_llama3.1_json.jinja`.
- `aurora-develop/aurora` `cbfbabc9` — `internal/toolcall/{prompt,parser,recover}.go`,
  `internal/handler/chat_handler.go`, `typings/chatgpt/request.go`.
- `izaart95-jpg/GLM-Free-API` `a958fc1caaa48edd755bb5b451b135f02526d041` —
  `internal/zbridge/agent.go`.
- `Godde3s/qwen-free-api` `71c2e65c8282eeb4b72965a7f11d09d8bb3c71e2` —
  `internal/qbridge/agent.go`.
- `NIyueeE/ds-free-api` `99f77302cebc0b32f10066f96b863f2f27752708` —
  `src/openai_adapter/request/{tools,prompt}.rs`,
  `src/openai_adapter/response/tool_parser.rs`.
- `lanqian528/chat2api` `a63a90ce` — negative result: no tool calling.
- `songquanpeng/one-api` `8df4a267`, `Calcium-Ion/new-api` `206e0fa5` —
  drop-or-native / diagnostics-only, the anti-pattern baseline.
- Anthropic tool search tool — https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool
  (30–50 tools, >85 %, 3–5 loaded, at least one non-deferred).
- OpenAI tool search — https://developers.openai.com/api/docs/guides/tools-tool-search
  ("mostly deferring the parameter schema").
- Claude Code MCP tool search — https://code.claude.com/docs/en/mcp#scale-with-mcp-tool-search.
- arXiv 2605.24660 (shortlist depth), 2510.14537 (JSPLIT), 2503.01763 (ToolRet),
  2410.14594 (Toolshed), 2610.14334 (MCP metadata interventions).
- Local project evidence: `/root/inception-dig/TOOLCALL-PATH.md` and
  `probe/outTC/tc2|tc3|tc4/body.bin`.
