# TASK: Adversarial Review — Dead Code, Conflicts, Placeholders, Logic Gaps

## Target

/root/inceptionlabs2api (app/ Go module; repo pushed to bacteriaw/main). Full
adversarial review, same spirit as the chatglm2api review. Five passes:

1. **Dead code** — unused functions/types/constants/branches across all packages
   (config, clear, pool, upstream, openai, server). Delete only what is provably
   unreferenced (tests may be updated). Check especially: leftovers from the
   utls era (fp removal might have left stragglers — flag names, helper funcs,
   config knobs that no longer do anything), v2-era fields the pass-through
   philosophy orphaned (e.g. the no-op IL2API_CONTEXT_CHARS knob — verify
   whether it should be deleted or documented), sentinel/event constants never
   emitted, error types never constructed.
2. **Logic conflicts** — contradictions between packages or between SPEC and
   code: config defaults vs docs vs README; validation rules that survived the
   pass-through purge but shouldn't have; the clearance manager vs pool vs
   server error-mapping disagreeing on retry semantics; SSE framing
   inconsistencies between chat/web_search/audio endpoints (each was added in
   a different pass — audit for uniformity: error envelope shape, finish
   behavior, watchdog parameters, header setting order); SPEC sections that
   describe superseded behavior (the spec had v1/v2/v3/v4/v5 edits — check
   every section against the current code).
3. **Placeholders / unimplemented** — grep the entire repo for TODO, FIXME,
   XXX, "not implemented", "for now", "placeholder", "stub", empty function
   bodies, always-nil branches, config knobs parsed but never read, response
   fields constructed but never populated. Every hit: real gap or intentional?
   Fix the real ones.
4. **Test audit** — tests that assert superseded behavior (v3/v4/v5 rewrites
   may have left tests checking removed behavior or duplicating coverage);
   tests marked skip; flaky timing-dependent tests.
5. **Race/leak pass** — go test -race full suite; go vet; a quick manual review
   of goroutine lifecycles (clearance refresh loop shutdown, pool fill loop,
   per-request contexts, the SSE writer paths — abort/disconnect handling in
   the three endpoint families).

## Method

- TDD for every fix: failing test first (or existing test adjusted), fix, green.
- Atomic English commits, each: go build ./... && go vet ./... && go test -race ./... .
- Update docs/SPEC.md where it disagrees with reality (the SPEC is the design
  of record — code wins only if the change was itself reviewed; otherwise fix
  the code to match the SPEC).
- Live probing: max 3 requests, 20s apart (healthz + one chat + one search),
  localhost only. No service restarts needed; do not leave any process running
  beyond the audit.

## Deliverables

1. docs/REVIEW-2026Q4.md — findings ledger: every issue, severity, root cause,
   fix applied or why left alone, file:line refs. Include the placeholder
   grep results in full with verdicts.
2. Atomic commits.
3. Final report: issues by severity, dead-code inventory deleted, conflicts
   resolved, SPEC-vs-code corrections, and confirmation the suite is green.
