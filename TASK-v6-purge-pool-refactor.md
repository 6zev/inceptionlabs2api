# TASK: Pass-Through Purge Round 2 + Cookie Pooling + Dedup Refactor

Three directives from the user:

> 清理掉那些我们主动自以为是拦截的，应该让上游自己会报错返回
> 我们应该提前预热一批cookie或挑战，轮询什么的
> 那些重复多余的，看是要砍掉，还是需要重新合理设计优化流程。那些重复的是否需要重构重写整理代码

## Part 1 — Purge remaining invented rejections (client-side validation that the upstream answers better)

The v3 purge removed the big ones, but the review and five feature passes added
new surfaces. Sweep EVERY request path again with the rule: the gateway rejects
ONLY (a) unparseable JSON, (b) structurally impossible requests (no messages /
no query / no text at all, wrong JSON types), (c) auth failures. Everything else
flows upstream and the upstream's error is translated into the envelope.

Known suspects to re-examine (verify against the upstream's ACTUAL behavior,
then remove or relax — cite a probe or the dig evidence for what upstream does):
1. **TTS voice whitelist → 400 on unknown voice.** The upstream answers an
   unknown voice with a silent 0.44s clip — a bad-but-valid 200. We 400'd it.
   Decision: this one is arguably client-visible-friendlier, BUT it is still an
   invented rejection. The honest option per the directive: PASS IT THROUGH and
   let the client see the upstream's behavior (document the 0.44s quirk in
   README). If you disagree, argue it in the SPEC with a written rationale and
   keep the 400 — but the default direction is pass-through.
2. **web_search caps**: 32 queries / 50 sources / bounded strings, http(s)-only
   URL filter, `fanout/depth/keep_top_k` clamping vs rejecting. Which of these
   are genuinely structural (needed to keep OUR response shape valid) vs
   invented? e.g. URL filtering protects the annotation shape — probably keep,
   but justify. Sources cap 50: upstream sends what it sends — do we really
   need to cap, or can we pass through (and only cap if the response would
   explode)? Examine each, keep only what protects structural integrity, drop
   the rest.
3. **Audio 25 MiB cap → 413.** Upstream's own limit is ~1 MB on decide and
   unknown on stt. Is our 25 MiB the upstream's answer or ours? If upstream
   413s earlier, drop ours and let it. If upstream hangs on big bodies, keep
   (documented) — but find out which it is (one probe allowed: a ~2MB file).
4. **Search config probe gating** (search disabled if probe says unavailable):
   if the probe result is stale/wrong (the v4 bug showed this class of failure),
   should a request still TRY the upstream and get the upstream's error?
   Prefer: probe is advisory (healthz display) only; requests always flow.
5. **Anything else**: sweep handler validators for invented rules (field
   allowlists, model name checking on chat — is `model_not_found` OUR check or
   upstream's? if ours: mercury-3 is the only model; passing an unknown model
   upstream gets... test what upstream does with garbage model? chat has no
   model field upstream — our /v1/models is a static list, so unknown-model 400
   is part of OUR protocol surface. Argue it: OpenAI clients expect model
   validation; keep with rationale, or map everything to mercury-3 silently.
   Decide and document.)
Each removal: adjust tests (the 400 tests become pass-through tests with mocked
upstream behavior), SPEC rows updated, README notes where user-visible.

## Part 2 — Cookie/clearance pooling & prewarm batch

Current: ONE clearance cookie, boot-prewarmed, refreshed at 45min, re-acquired
on 429. The user wants a BATCH: prewarm several cookies and rotate.

Design questions to resolve (SDD first — update SPEC §2.1.x):
1. Does the upstream bind `_vcrcs` per-IP-per-UA only (established), i.e.
   would multiple simultaneous cookies even differ? One acquisition = one
   solve (~100-400ms) + one POST. If cookies are fungible, a pool of N
   identical cookies protects against: per-cookie revocation on the edge
   (does Vercel ever single out one clearance? unknown), and spreads the
   request-per-cookie rate the edge might score. Evidence first: acquire 2-3
   cookies and test whether either works interchangeably and whether using
   cookie A on many rapid requests gets A rejected while B survives (≤6
   probes, 15s spacing — this is the core evidence for the design).
2. Implement per evidence:
   - If cookies are fungible and no per-cookie penalty: a small ring (size
     from config, default 3), boot prewarm fills all, rotation is
     round-robin per REQUEST (not per token), refresh at the 45min margin
     per slot, 429 on a slot retires that slot and re-acquires, other slots
     unaffected. /healthz reports per-slot age + refresh_count.
   - If cookies are per-IP-unique (same value every time): then a "pool"
     is meaningless; instead prewarm stays as-is and the design reduces to
     documenting why (SPEC note). Don't build a fake pool.
   - If per-cookie penalties exist: keep the ring but add the mitigation the
     evidence suggests.
3. Also prewarm the SESSION POOL concurrently at boot (it already does) —
   verify boot order: clearance first (pool fetch depends on it), then pool,
   then search probe. Measure and document cold-boot time to fully-warm state.

## Part 3 — Dedup & refactor audit

The five feature passes (chat → web_search → audio → citations → review) each
added their own upstream-call plumbing. Audit for duplicated/multiplied logic
and either CUT or consolidate:
1. `postAuthed` retry loop (401/403 → new token → retry ≤2): is it shared by
   chat, search, tts, stt, and the probe, or reimplemented per family?
   Consolidate into ONE upstream call wrapper (with per-call body/decoder
   injection) if duplicated.
2. Error translation to the OpenAI envelope: how many copies? One mapper.
3. Idle watchdog / timeout scaffolding per family: chat (SSE), search
   (NDJSON), audio (whole-call): is the timing/abort logic shared or
   triplicated? Consolidate the mechanism (deadline context + sliding reset)
   into one internal helper with per-family parameters.
4. Clearance-cookie application to requests: every family must go through the
   same Transport/cookie header logic — verify no family hand-rolls headers.
5. Sentinel/event-constant sets, URL/path constants, config struct sprawl —
   dedupe.
6. Where consolidation is done: the SPEC gets a short §"internal architecture"
   describing the unified pieces; tests stay per-family (black-box) plus unit
   tests for the shared helpers.
Rule: behavior must NOT change (this is refactor, not redesign) — except where
Part 1/2 deliberately changed it. Pin behavior with the existing tests before
refactoring; adjust only what Part 1/2 changed.

## Method

SDD: SPEC updates per part (pass-through table rows, cookie-ring design with
evidence, architecture section), commit before implementation. TDD throughout.
Atomic English commits, each green: go build ./... && go vet ./... && go test
-race ./... . Probes: ≤8 total (Part 2 evidence ≤6 + Part 1 stt-size ≤1 +
optional model-garbage ≤1), ≥15s apart. Final live smoke: 3 requests (healthz,
chat, one affected path) — that's within the 8 or +3, state which. Push to
origin main. README/SPEC updates for everything user-visible. English.
