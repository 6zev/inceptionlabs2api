# TASK v2: /v1/web_search — bridge /api/search-lab/run

## Ground truth (from docs/RESEARCH-websearch.md — read it first)

The upstream has a STANDALONE retrieval endpoint used by the official /search playground:
- `POST /api/search-lab/run`, body `{query, config:{fanout 1–5, depth 1–3, keepTopK, effort}, timezone, language}`
- Response: NDJSON stream (`application/x-ndjson`), stages plan → retrieve → rerank → followup → synthesize; the `docs` event carries `sources:[{title,url,score}]`
- `GET /api/search-lab/config` advertises `retrievalAvailable` + parameter limits
- NDJSON body may be Brotli-encoded: the client MUST send `Accept-Encoding: identity`
- There is also an inline chat path (`webSearchEnabled`) with `__searching__` and `__search_error__` sentinel titles — OUT OF SCOPE for this task, except: add `__search_error__` to the chat stream's ignored sentinel list if not already handled (one small guard, with test).

## Build (in /root/inceptionlabs2api, repo already pushed to bacteriaw/main)

New endpoint `POST /v1/web_search` shaped like simple-chat's (see
https://github.com/Sliverkiss/simple-chat — app/internal/server/websearch_endpoint_test.go
for the exact response contract):
```json
{"query": "...", "config": {"fanout":3, "depth":1, "keep_top_k":10, "effort":"medium"}}
→ 200 {
  "object": "web_search",
  "queries": [<actual queries from the plan events>],
  "results": [{"url","title","snippet","cite_index"}]
}
```
- Reuse the existing pool/fingerprint transport from the chat path.
- Parse the NDJSON: collect queries from `plan`, sources from the `docs`/sources event
  (title/url/score → snippet left empty or from source metadata if present,
  cite_index = 1..N in rank order). Do NOT wait for synthesis unless needed for
  snippets — if per-source snippets only exist post-synthesis, capture them there;
  document the choice in the SPEC.
- Non-200 from upstream / `retrievalAvailable:false` → error envelope, 503/504 mapping
  consistent with the chat path. Empty query → 400. Timeouts: 60s sliding watchdog,
  same as chat.
- Startup healthz: optionally probe /api/search-lab/config once, expose
  `search_available` in /healthz (cache the result; never block boot on it).
- Tests (TDD): httptest mock of /api/search-lab/run covering full pipeline NDJSON,
  brotli-encoded body, missing sources, config-disabled, timeout; endpoint tests for
  the simple-chat response shape. NO live probing needed beyond at most ONE
  verification request against the real endpoint IF the transport works from this
  host (it currently does not — plain probes get Vercel 429; skip live if so, mock only).

SDD: update docs/SPEC.md with the web_search section first (commit), then implement.
Atomic commits, English, each green: go build && go vet && go test ./...
Push to origin main when done. README: add the /v1/web_search section (Chinese).
