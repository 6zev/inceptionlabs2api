# TASK v7: Async Concurrent Clearance Prewarm (chatglm2api-style) + Pool Default 20

## Directive from the user

> 这个预热填充的，可以参考我们的chatglm2api吧，不都是类似于匿名吗，可以异步之类的，不用卡着进程
> 3个cookie预热太少了，20个，429了就可以丢弃然后重新解pow填充了吧

## Current state (verified)

- `IL2API_CLEARANCE_POOL` default is being raised 3 → 20 (already changed in
  config.go; tests/README/SPEC may still say 3 — sync them).
- `PrewarmNow` fills slots SEQUENTIALLY (SPEC §2.1.2's "concurrent solves are the
  storm the cooldown exists to prevent"). With 20 slots at ~100-400ms solve +
  a network round trip each, boot blocks ~10+ seconds inside the prewarm
  goroutine and the ring fills slowly one-by-one. (Boot itself doesn't wait,
  but the RING fills serially, and the first requests may hit the
  single-flight acquireSlot path.)
- `acquireSlot` has a GLOBAL single-flight (`c.inflight` + one shared
  `inflightDone`): while one acquisition is running, every other slot's fill
  just waits on it. At 20 slots this serializes everything.
- The reference design: chatglm2api's identity pool (/root/chatglm2api,
  internal/pool) — concurrent warm fill, partial availability serves traffic,
  background completes the stock, per-slot independence.

## Design change (SDD first — update SPEC §2.1.2)

1. **Concurrency analysis, honestly**: the PoW solve is LOCAL CPU work — it is
   not upstream traffic at all. The only upstream calls in acquisition are
   fetchChallenge (GET) and submitSolution (POST). So:
   - Solve PoW for up to N slots CONCURRENTLY (local goroutines, bounded,
     e.g. all 20 — CPU is trivial).
   - SUBMIT solutions with a small per-request spacing (e.g. 250ms between
     submitSolution calls) to stay WAF-civil — that's 20 submits spread over
     5s at boot, far gentler than the request traffic itself.
   - The SPEC's "no concurrent solves" line was about submission storms;
     rewrite it to say exactly that: solves are local and concurrent,
     submissions are paced.
2. **Prewarm semantics**:
   - Boot: spawn the fill; return as soon as at least ONE slot is live (the
     gateway can serve); the rest of the ring continues filling in the
     background with paced submissions. (chatglm2api-style: partial
     availability serves, background completes.)
   - Full fill target: all 20 slots.
   - Failures: a slot that fails to fill is retried by the existing
     Refill/RefreshStale loop; prewarm logs and moves on (never blocks boot).
3. **Per-slot single-flight** instead of global: a slot being filled doesn't
   block other slots' fills or other slots' use. Keep the cooldown semantics
   (a slot that just failed waits out the cooldown before its next attempt),
   but the cooldown must be PER-SLOT too (the v6 review already moved cooldown
   to failures-only — verify it's per-slot, fix if it's global).
4. **429/retire path** (user asked to confirm): Retire(cookie) drops the slot
   and a background refill re-solves PoW and refills THAT slot. Verify this is
   already per-slot and asynchronous; if the refill currently runs inline on
   the request path, move it to a goroutine so the rotated request proceeds on
   another slot without waiting.
5. **healthz**: slots[] already per-slot; make sure a "filling" state is
   distinguishable if it exists (or leave absent = not-yet-live).
6. Default 3 → 20 everywhere: config default (done), README table line,
   SPEC §2.1.2, any test fixtures with 3, docker-compose/.env examples.
   Also verify ClearRefreshLoop's "one per tick" policy still makes sense at
   20 slots (45min refresh of 20 slots at one-per-tick — check tick interval
   math; adjust the per-tick replacement limit so the full ring refreshes well
   within the 45min window without bursts).

## Method

SDD: SPEC §2.1.2 rewrite first (commit). TDD: tests for concurrent fill
(fast-fill of N slots with a mock, paced-submission assertion, boot-returns-
after-first-slot, per-slot cooldown, retire→async refill, refresh-loop
completes 20 slots in-window). Then implementation. Adjust existing tests
that assumed sequential fill or global single-flight.

Atomic English commits, each green: go build && go vet && go test -race ./...
Live verification: restart the local :3000 instance with the new binary and
CONFIRM via healthz: ring reaches 20/20, boot-to-first-slot time, no 429s
during fill. That's it — no other live traffic. Push to origin main.
README/SPEC/config examples synced. English.
