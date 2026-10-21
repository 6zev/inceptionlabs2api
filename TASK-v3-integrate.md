# TASK v3: Integrate the Verified Vercel Bypass + Remove Presumptuous Interception

## Part A — Replace the transport with the PROVEN solution

Ground truth: /root/vercel-bypass/REPORT.md + the working PoC at
/root/vercel-bypass/poc/main.go (pure stdlib Go, verified live: 200 on
/api/session from this host). Facts established there:

- The old design is WRONG and must be DELETED ENTIRELY: utls ClientHello, the
  forced ALPN http/1.1, the "h2 is rejected" claim (backwards — h2 is REQUIRED).
- The proven minimal condition: stdlib crypto/tls with HTTP/2, Chrome UA,
  and the `_vcrcs` clearance cookie.
- Cookie acquisition (fresh egress): GET challenge → solve v2 PoW locally
  (3-round SHA-256 partial-prefix chain, ~100ms) → POST request-challenge →
  204 + `_vcrcs` (1h TTL) → proceed. The PoC code is the reference — port it.
- The cookie is bound to egress IP + UA; acquisition must run through the SAME
  egress the gateway uses (so through the same proxy setting if one is set).
  No external IP refresh is needed — the solver runs inline.

Rebuild `app/internal/fp/` (or replace it with a new package, e.g. `internal/clear`):
1. Port the PoW solver from the PoC verbatim (with its captured-fixture tests).
2. Clearance manager: lazily acquire on first need; refresh when older than a
   safety margin of the TTL (e.g. refresh at 45min of the 60min TTL); re-acquire
   immediately on a 429 challenge response; track last-good cookie. Thread-safe.
3. Transport: stdlib http.Client, ForceAttemptHTTP2, fixed Chrome UA header.
   Proxy support: keep the existing HTTPS_PROXY-style config (cookie acquisition
   and requests share the same Transport).
4. The session pool keeps its role (tokens for /api/chat), but session FETCHES
   now go through the clearance-aware client too.
5. DELETE: the utls dependency (go.mod must drop it — the project then has ZERO
   non-stdlib deps, update SPEC/README claims), ALPN forcing, any h1.1 logic.
6. /healthz: report clearance state (has_cookie, age, refresh_count).
7. Tests: port the PoC's solver fixture tests; clearance-manager unit tests with
   mock challenge endpoints (mock the challenge page, request-challenge 204,
   cookie set, expiry, 429 re-acquire path). NO live probing beyond ONE final
   smoke (GET /api/session → expect 200) since the bypass is now proven — if
   that smoke fails, STOP and report rather than patching blindly.

## Part B — Remove self-invented interception (user's directive)

> 我们很多时间不需要自以为是的拦截吧 — upstream errors will speak for
> themselves. The gateway must TRANSLATE, not second-guess.

Audit the whole codebase for presumptuous client-side validation and remove
what the upstream can answer better itself. Known suspects (verify each
against the real upstream contract, then remove or relax):

1. The explicit 400 rejection of unknown OpenAI params (temperature, top_p,
   n, logprobs, response_format, presence/frequency_penalty, seed, user...):
   DELETE the rejection. Silently DROP the fields instead (they have no
   upstream meaning) — clients like OpenAI SDKs send them routinely and a 400
   breaks innocent callers. Keep validation only for what is structurally
   unparseable (malformed JSON, no messages, wrong types).
2. The empty-tool_calls 400 saga: since tool calling is out of scope, requests
   WITH tools should simply proceed as plain chat (tools dropped), not error.
   Same for tool_choice. (If this was already relaxed in the earlier hardening
   commit, verify and finish the job.)
3. The 250k context guard → REMOVE the 400. The upstream truncates silently at
   its own limit; that is the upstream's contract. (If we want, keep an env-
   opt-in cap, default off — but default behavior = pass through.)
4. Image/multipart content parts in messages: currently a 400. Upstream will
   error on what it can't accept; let it. Convert text parts and pass the rest
   as text if trivially possible, otherwise let upstream respond. (Keep ONLY
   the structural check that a message has SOME renderable text.)
5. Any other invented rejections: e.g. body-size caps stricter than what the
   upstream enforces, header allowlists, param allowlists. Sweep and prune.
   RULE: the gateway rejects ONLY (a) malformed JSON, (b) structurally invalid
   requests (no model/messages at all, wrong JSON types), (c) auth failures.
   EVERYTHING else flows upstream and the upstream's error is mapped into the
   OpenAI error envelope unchanged in message/content.
6. Keep genuine safety: API-key auth, request timeouts, idle watchdog, and
   the disconnect-safe pool stay untouched.

SDD: update docs/SPEC.md first (transport section REWRITE + a new "pass-
through philosophy" section enumerating what we do and do NOT validate),
commit, then TDD. Adjust the affected tests (the 400-param tests become
drop-silently tests, the context-guard test becomes pass-through, etc.).

Every commit green: go build && go vet && go test ./... (race). Atomic English
commits. Push to origin main. README updates: transport section (h2+cookie
bypass story), removed-validation notes, zero-dependency claim. One final live
smoke of the full stack (boot → /healthz → one chat completion → one web_search)
is AUTHORIZED and expected; keep it to that.
