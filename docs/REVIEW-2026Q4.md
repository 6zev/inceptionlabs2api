# Adversarial review — 2026 Q4

Scope: the whole Go module in `app/` (config, clear, pool, upstream, openai,
server, main), the deployment files, and `docs/SPEC.md` against the code.
Five passes: dead code, logic conflicts, placeholders, tests, races/leaks.
Every fix was written test-first and landed as its own commit; each commit
passed `go build ./... && go vet ./... && go test -race ./...`.

Ledger legend — **S**everity: CRITICAL / HIGH / MEDIUM / LOW.
Status: *fixed* (commit), *left* (with reason), *doc* (SPEC/README corrected).

---

## 1. Logic conflicts

| # | Sev | Finding | Root cause | Fix |
|---|-----|---------|------------|-----|
| L1 | HIGH | `IL2API_AUDIO_TIMEOUT` bounded only the pre-header phase. TTS/STT answer `200` and then stream the body, so a stalled upstream held the handler open with **no watchdog at all** — the only request path in the gateway with an unbounded phase. SPEC §7.2/§7.3 promise `504 upstream_idle_timeout` on an upstream timeout, which was unreachable. | The audio client passed the timeout to `postAuthed`'s header timer, mirroring chat — but chat has the sliding reader watchdog behind it and audio does not. | Whole-call `context.WithTimeout`; a body-read expiry is classified as `ErrIdleTimeout` so the server maps it to `504`. Tests stall the body after `200`. `dd1eb8f` |
| L2 | HIGH | Clearance cooldown applied **only while a cookie was held**. A checkpoint that kept refusing was re-solved by every concurrent caller — the exact solve storm the cooldown exists to prevent (SPEC §2.1.1) — and inside the window `Ensure` returned `nil` with no cookie, letting an unprotected request proceed. | `refresh()` gated on `has` (cookie present) instead of on the previous *outcome*. | Failures open the window; a caller without a cookie inside it gets the recorded error. Successful solves never open a window, so the 429-replay recovery path stays unblocked. `d4788af` |
| L3 | MEDIUM | On an unchallenged egress, `Ensure` had no "already probed" state: `Send` runs `Ensure` before **every** upstream request, so each one performed a fresh `/api/session` probe — doubling session traffic the pool already generates. | `refresh()`'s `errNoChallenge` branch returned success without recording anything, so `staleLocked()` was always true for an empty cookie. | Record the probe result with its timestamp; re-probe on the same schedule a cookie would be refreshed; `Invalidate` clears it. `0444a75` |
| L4 | MEDIUM | The three POST JSON routes disagreed on an unparseable-trailing-data body: chat accepted trailing **valid** JSON (`{"x":1}`) and reported trailing *junk* as an opaque unmarshal error, while web_search and audio rejected both as "unexpected data after the request object". | Chat's decoder predates the other two; it only tested `err == nil` on the second decode. | Chat uses the same `!errors.Is(err, io.EOF)` rule and message as its siblings. `092e062` |
| L5 | LOW | `/v1/chat/completions` answered `405` without `Allow: POST`, while search and both audio routes set it. | Endpoints written in different passes. | Uniform `Allow` + a table test over all four paths. `d2b9056` |
| L6 | LOW | Search bounds were documented (SPEC §2.3.2) and enforced in **runes** by the citation collector, but the search collector compared `len()` (bytes): a 1 500-character CJK query or CJK-path URL was dropped as oversized. | Two collectors implemented the same rule twice. | Compare rune counts on both search paths. `9aea92e` |
| L7 | HIGH (deployment) | `docker-compose.yml` set `IL2API_CONTEXT_CHARS=250000`, resurrecting a knob the code calls deprecated and advertising a 250k input guard SPEC §1.1 says does not exist. The same file omitted `IL2API_CLEARANCE_REFRESH/COOLDOWN` and both audio variables, so they could not be set through compose at all. | The knob block predates the pass-through purge; the newer knobs were never added. | Remove the dead knob, pass the four missing ones; test over the deployment files enforces "no non-zero deprecated knob". `ba113c5` |
| L8 | HIGH (deployment) | `Dockerfile` ran `COPY app/go.mod app/go.sum ./`. The module has zero dependencies so `go.sum` does not exist; **the Docker build fails on a clean checkout** (`"/app/go.sum": not found`), which is the documented deployment path. CI's docker job would catch this only after a push. | The go.sum was removed when the dependency was dropped; the COPY was not. | Drop the copy; a test verifies every context COPY source exists. Verified with a real `docker build`. `8471983` |
| L9 | MEDIUM (test) | `TestEndToEndClearancePrewarmAtBoot` waited one second for the boot cookie. Under `-race` with the full suite in parallel the PoW solve + TLS handshake routinely exceed it: **5/5 full-suite `-race` runs failed**, while the test passed in isolation. | Latency asserted instead of the property (non-blocking start, eventual cookie). | Keep the "start is immediate" assertion, widen the acquisition deadline to 10 s. `8471983` |
| L10 | MEDIUM (test) | `TestChatHeaderTimeoutIsBounded` passed when `Chat` returned `nil` (a silent success) and when it returned a `*StatusError` — it could not fail for the behaviour it names, and never asserted the typed error the server maps to 504. | Assertion wrapped in `if err == nil \|\| errors.As(...)`, i.e. skipped on both the bug and the success case. | Warm the pool, require `ErrIdleTimeout`. `bc10ec0` |
| L11 | LOW | The 60 s idle watchdog is applied by `Reader` **only after** `Read` is called; the header bound is separate and correct. No gap found — recorded here because the task asked for the check. | — | *left*: verified `do()` bounds dial/TLS/headers and `Reader` bounds the body. |
| L12 | LOW | SSE framing across the three families: chat is the only stream (search and audio are whole-body). All non-stream families use the same `{"error":{"message","type","code","param?"}}` envelope; chat's mid-stream frame uses the identical nested shape followed by `data: [DONE]`. 413/401/405 bodies are uniform (verified by probe and by the new table test). | — | *left*: consistent; no divergence to fix. |

## 2. Dead code (deleted) — inventory

| Identifier | Where | Why dead | Commit |
|---|---|---|---|
| `ChromeUA` | `internal/upstream/client.go` | Duplicate of `clear.UserAgent`, zero readers; the binding invariant is documented on the original. | `5d7addc` |
| `Client.Logf()` | `internal/upstream/client.go` | No production caller (only its own test); the server receives the logger via `Config.Logf`. | `5d7addc` |
| `Event.ID` | `internal/upstream/events.go` | Parsed never read: nothing keys off block ids. | `5d7addc` |
| `pool.ErrInvalidToken` | `internal/pool/pool.go` | Exported but never returned (`ValidateToken` returns a bool). | `5450a8a` |
| `refillTimeout` (unused) | `internal/pool/pool.go` | Declared with a doc comment, then bypassed by a hard-coded `15*time.Second` in `Discard`. | `5450a8a` (now used) |
| `server.Config.ContextChars` + its wiring in `main.go` | `internal/server`, `main.go` | Parsed and copied into the HTTP layer, never read. Kept in `config` only for boot compatibility with old deployment files. | `f735a43` |
| `IL2API_CONTEXT_CHARS` in `docker-compose.yml` | deployment | Non-zero no-op default. | `ba113c5` |
| `_ = errors.Is` and dead no-op loops | tests | Leftover scaffolding. | `bc10ec0` |

**Judged not dead (kept, with reason):**

- `EventReasoningStart`, `EventReasoningEnd`, `EventTextEnd` — never compared
  in production (those events produce no frame by design, SPEC §3.2), but they
  are the named vocabulary the reader tests and fixtures assert against; they
  document the protocol the package understands. Deleting them would turn
  name checks into bare strings. *left*.
- `Config.Pool` / `BindPool` — `Config.Pool` is the injection point tests and
  embedders use; `BindPool` is required at startup because the pool depends on
  the client's fetcher. Both exercised. *left*.
- `pool.Stats.Expired` — read by tests and serialised on `/healthz` only in
  the sense that `Stats` is; it is a real operational signal. *left*.
- `Event.SourceID` — used for sentinel detection and asserted by the capture
  tests. *left*.
- `SearchSource.Score` — parsed and kept on the wire type; the response
  contract deliberately does not expose it (simple-chat compatibility), but
  it is asserted by the collector tests. *left*.
- `AudioUpload.ContentType` — an override hook used by tests and available to
  embedders; the sniffer supplies the value otherwise. *left*.

## 3. Placeholders / unimplemented

Full grep (`TODO|FIXME|XXX|not implemented|unimplemented|for now|placeholder|stub`,
case-insensitive, Go + YAML + Dockerfile + Makefile + env examples):

```
app/.env.example:53:# deliberately not implemented: the upstream only decodes MP3 (SPEC §2.5.2).
app/internal/openai/types.go:20:// placeholders instead of being rejected.
app/internal/openai/messages.go:19:// become "user", and media parts become short text placeholders
app/internal/openai/messages.go:62:// A non-text part is rendered as a short placeholder naming its type
app/internal/openai/messages.go:88:if placeholder := mediaPlaceholder(typ); placeholder != "" {
app/internal/openai/messages.go:89:texts = append(texts, placeholder)
app/internal/openai/messages.go:98:// mediaPlaceholder is the text stand-in for a non-text content part.
app/internal/openai/messages.go:99:func mediaPlaceholder(typ string) string {
app/internal/openai/decode_test.go:174:// text placeholder so the model still sees that something was sent.
app/internal/openai/decode_test.go:175:func TestNormalizeRendersMediaAsPlaceholder(t *testing.T) {
```

Verdicts: every hit is the *implemented* media-placeholder feature or the
deliberate no-transcode decision — no hit is an unfinished body, a stub, or a
TODO. Zero `TODO`/`FIXME`/`XXX` in the repository.

Knobs parsed-but-never-read: `IL2API_CONTEXT_CHARS` was the only one; it is
now read nowhere outside `config` (kept there for boot compatibility) and no
longer shipped as a non-zero default (L7, §2 above). All 19 documented
variables are consumed.

Constants declared but never emitted: none besides the event-vocabulary
constants judged above. `pool.ErrInvalidToken` (never returned) was deleted.

Functions with always-nil branches: none found. `SearchConfig` intentionally
rejects a body without `retrievalAvailable` rather than reading it as `false`
(tested).

## 4. Test audit

- **Vacuous assertion fixed**: `TestChatHeaderTimeoutIsBounded` (L10).
- **Flaky/latency-bound test fixed**: `TestEndToEndClearancePrewarmAtBoot`
  (L9), reproduced 5/5 under full-suite `-race`.
- **Blocking callback hazard**: `TestMaintainerReportsErrors` used a 1 ms
  ticker and an unbuffered-select-free channel send; if the test stopped
  draining, `Close` could deadlock. Callback is now non-blocking.
- **Superseded behaviour**: none. Tests that assert the pass-through purge
  (`TestPresumptuousRejectionsAreGone`, `TestLongInputPassesThrough`,
  `TestNormalizeHasNoLengthGuard`) are current and intentionally named after
  the removed behaviour.
- **Skipped tests**: none (`grep t.Skip` → 0 outside the Dockerfile-context
  guard added here, which skips only when the Dockerfile is outside the test
  context).
- **Timing-dependent tests**: the remaining `time.Sleep`s are bounded polls
  with deadlines, not fixed-latency assertions. `-count=10 -race` over
  `clear`, `upstream`, `server`, `pool` was green before the review; the full
  suite was then run 5× green after L9.
- Coverage after the review: 90.8% statements (README updated).

## 5. Races and leaks

- `go test -race ./...` green, repeated 5× in full and 10× per
  timing-sensitive package.
- **Goroutines**: a filtered probe (only stacks containing
  `inceptionlabs2api/internal/`) run after `StartClearancePrewarm` + 20
  `Get`s + `cancel()` + `pool.Close()` showed **zero** surviving gateway
  goroutines. `RunRefreshLoop` exits on `ctx.Done`; the pool maintainer exits
  on `Close` (`sync.Once` + `WaitGroup`); `Reader`/`collectSearch` scanner
  goroutines are released via `done` and the caller's `defer body.Close()`.
- **Per-request contexts**: `do()` creates a `context.WithCancel` for the
  header timer, attaches `closeHook` to release it when the body closes, and
  every caller defers `Close`. Audio now additionally cancels its whole-call
  timeout on return (L1).
- **SSE abort**: chat stops the read loop on write failure (`errClientGone`)
  and on client cancellation without touching the pool; verified by
  `TestEndToEndClientDisconnectKeepsPool`.
- **Pool fill**: `fetchMu` serialises synchronous and background fills;
  `Discard`'s refill is bounded by `refillTimeout` (now actually used).

## 6. Live probing

Three requests, ≥20 s apart, localhost only, against a binary built from the
reviewed tree on `127.0.0.1:3100` (a pre-existing `:3000` process was left
untouched):

1. `GET /healthz` — clearance `has_cookie:true` (solved in 413 ms),
   `pool.size:2`, `search.available:true`.
2. `POST /v1/chat/completions` (non-stream) — `finish_reason:"stop"`, exact
   requested content, no `usage` field.
3. `POST /v1/web_search` (`fanout:1 depth:1 keep_top_k:3`) — rewritten query,
   three ranked sources with `snippet:""` and `cite_index` 1..3.

The probe process was stopped and its port confirmed closed; nothing from the
probe is left running.

## 7. SPEC-vs-code corrections

| SPEC | Was | Now |
|---|---|---|
| §7.1 | pointed at `app/internal/server/websearch_endpoint_test.go`, which does not exist | names `TestWebSearchResponseShape` in `search_test.go` |
| §2.1.1 lifecycle | cooldown spaces every refresh | spaces failures only; success never blocks the 429 replay; unchallenged egress is recorded, not re-probed |
| §2.5.1 body read, §7.2/§7.3 error tables, §8 | `IL2API_AUDIO_TIMEOUT` = pre-header bound | whole-call bound; body expiry → `504 upstream_idle_timeout` |
| README config table | cooldown "minimum interval between solves" | "minimum retry interval after a **failed** clearance" |
| README healthz sample | counters without `tts`/`stt` | includes them |
| README dev section | ~88% coverage | ~90% |

No SPEC change was made to accommodate a behaviour change; every code change
was made to match the SPEC or to fix an internal contradiction the SPEC's own
acceptance criterion exposed (L1).

## 8. Left alone, deliberately

- 405-on-unknown-path: `/v1/*`-shaped unknown paths fall through to Go's mux
  404. No SPEC promise exists for them; inventing one is out of scope.
- `mediaPlaceholder` accepts both `image_url` and `image` (and audio/file
  aliases). Both spellings are real in the wild; SPEC documents `image_url`
  and says "any other part type" renders.
- `Search` treats a body that closes after a `docs` event as complete, even
  without `run_done`. SPEC §2.3.2 states this explicitly ("docs is the
  product"); the synthesized answer is never wanted.
- `postAuthed` returns the last 401/403 error as bare `ErrAuthRejected` with
  no upstream message. Deliberate: the upstream body for an auth rejection is
  not a message a client can act on, and SPEC §3.4 maps it to
  `upstream_auth_failed`.
