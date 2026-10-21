# TASK v4: Search Citations + Cookie Prewarm

## Part 1 — source-url citations (fix the orphan reference markers)

Live test found: with webSearchEnabled (now default true) the model's answer text
contains inline citation markers like [13], [19], but the gateway DROPS the upstream
`source-url` SSE events, so clients get numbered references with no sources (17
orphan markers in one real answer).

Fix per OpenAI conventions:
1. Parse `source-url` events in the upstream SSE reader. Capture the full event
   payload shape live first (one authorized live probe: stream a chat that triggers
   search and dump raw source-url events to docs/ — cite exact fields in the SPEC).
   Known from the audit: events carry ranked sources; also `__searching__` and
   `__search_error__` sentinel titles exist somewhere in the stream vocabulary —
   handle them (searching = no-op, error = log + continue).
2. Stream path: collect sources as they arrive; when the upstream text block
   finishes, emit one annotation-bearing chunk:
   `choices[0].delta.annotations = [{type:"url_citation", index: <1-based cite number>,
   url, title, start_ix, end_ix}]` — start/end at 0/0 if offsets are unavailable
   (do not invent). Alternative accepted: attach `annotations` to the finish chunk.
   Choose one, document in SPEC, tests pin the exact framing.
3. Non-stream path: `message.annotations` array with the same entries.
4. If the upstream source events do NOT carry a per-source index that maps to the
   text markers, use arrival rank order (matches how the model numbers citations
   in practice) and document the mapping assumption in the SPEC.
5. Sentinel `__search_error__`: if the stream signals search failure mid-answer,
   keep the text flowing; add a log line. Never abort the stream for search issues.

## Part 2 — Clearance cookie prewarm

Current: Clearance.Ensure() is lazy — first request pays the PoW + network round
trip. Fix:
1. On boot, start the clearance acquisition immediately (non-blocking goroutine;
   failure logs but does not block or crash boot — pool already behaves this way).
2. Keep the existing refresh-at-45min logic; it now runs from boot, not from
   first traffic.
3. /healthz: clearance should read `has_cookie:true` within a second of boot in
   the normal case. No new config knobs needed (keep it simple).
4. Optional single live smoke at the end: boot → healthz shows cookie → one
   streaming chat with search → assert annotations present in the response.
   That is the ONLY live traffic authorized (2 requests).

## Method

SDD: SPEC section first (citation wire format + prewarm semantics), commit; TDD:
mock source-url event streams in httptest (normal, error sentinel, missing index,
sources after text-end), pin stream/non-stream annotation shapes; prewarm tests
(non-blocking boot, failure tolerated). Atomic English commits, each green:
go build && go vet && go test ./... (race). Push to origin main. README: add
annotations to the feature list (Chinese, one line).
