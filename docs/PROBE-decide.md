# Probe report: `/api/decide/run` wire contract (2026-10-21)

Scope: nail the exact upstream wire shape before adding `POST /v1/decisions`.
Nine live probes, 20 s apart, through the proven PoW/`_vcrcs` transport
(`/root/inception-dig/probe`, plan `planD9.json` + `planE9.json`, captures in
`probe/outD9/` and `probe/outE9/`). Budget: 6 probes for the task; 3 were
re-spent after the first batch proved the request shape was wrong (see §1), so
9 were issued in total and that overrun is recorded here rather than hidden.

## 1. The client bundle was right; the docs' `v1/decisions` shape is not the web shape

The first batch (`outD9`) sent the shape the official docs use — `questions`
as a **map** of name → `{type, instructions, criteria}`. Every one of those
requests was rejected by the route's own validator:

| Probe | Body | Answer |
|---|---|---|
| d1-noul-min | map with one `noul` question | `400 {"error":"Add at least one question."}` |
| d2-choice-score | map with `noul`+`choice`+`score` | `400 {"error":"Add at least one question."}` |
| d3-image | `state` as chat-style parts + map questions | `400 {"error":"Give the model something to decide about."}` |
| d4-err-badtype | map, `type:"bogus"` | `400 {"error":"Add at least one question."}` |
| d5-err-state | 12 001-char state | `400 {"error":"Keep the state under 12,000 characters."}` |
| d6-err-badtoken | bogus `x-session-token` | `401 {"error":"Unauthorized"}` |

Reading the `/decide` playground bundle (`assets/chunks/0t1.js`) explains it:
the validator `N()` requires `Array.isArray(questions)` and then converts each
item into the map the server actually receives. The **web route takes the
UI's array shape** — `{state: string|parts, questions: [{id,name,type,
instructions,options}], images: [dataURL]}` — and the route itself echoes a
different response shape than the public API. The second batch (`outE9`)
sending the array shape succeeded on the first try.

## 2. Minimal run — the exact response

Request (exactly as the playground's `N()` builds it):

```json
{"state":"The customer was charged twice and asks for a refund.",
 "questions":[{"id":"q-1","name":"wants_refund","type":"noul",
               "instructions":"Is the customer asking for money back?","options":[]}],
 "images":[]}
```

Response: **HTTP 200, `Content-Type: application/json`, one complete JSON
object** (100 B, `latencyMs` 226 in-band; probe wall clock 20.2 s includes the
enforced spacing):

```json
{"answers":{"wants_refund":{"type":"noul","p":0.9990302478021369}},
 "inputTokens":72,"latencyMs":226}
```

Facts this pins:

- **Whole-body JSON, not a stream.** No `Content-Type: text/event-stream`, no
  NDJSON, no chunked framing. The route is non-streaming, and the official
  docs say the same thing about `v1/decisions` ("Streaming is not supported").
- The response has **no `model` field** and **no `usage` object**; the token
  count is `inputTokens` and the wall time is `latencyMs`, both at top level.
- `answers` is a map keyed by the question **name**. Answer field names are the
  web's (`p`, `choice`, `probabilities`, `confidence`, `score`, `levels`), not
  the public API's (`noul`, `legend`).
- `Cache-Control: public, max-age=0, must-revalidate` and `X-Vercel-Cache:
  MISS` are the deployment's page defaults; the gateway must not cache the
  body regardless (it derives from the caller's input).

## 3. Choice + score in one run

```json
{"answers":{
  "wants_refund":{"type":"noul","p":0.9990302479708626},
  "team":{"type":"choice","choice":"billing",
          "probabilities":{"billing":0.9999242600342508,
                           "technical":0.0000514409714130558,
                           "account":0.00002429899433603425},
          "confidence":0.9998863900513763},
  "urgency":{"type":"score","score":0.9858054357694235,
             "levels":[{"label":"Can wait a week","p":0.06417095154165331},
                       {"label":"Today","p":0.8858526611472699},
                       {"label":"Drop everything","p":0.04997638731107678}],
             "confidence":0.8287789917209049}},
 "inputTokens":251,"latencyMs":163}
```

- `choice.probabilities` is keyed by the option `name` **as the client sent
  it**; `choice.choice` is the arg-max name.
- `score.score` is a **fractional index** into `levels` (0.9858 of the way
  from level 0 to level 2 here), not an integer. `levels` is a positional
  array of `{label, p}` in the order the client sent `options`; `label` is the
  option's **description** (score options carry an empty `name` upstream).
- `confidence` is present on both `choice` and `score`, absent on `noul`
  (`noul` has only `p`).
- `inputTokens` scales with question count/state, so it is the upstream's own
  count. There is no output-token field on this route.

## 4. Errors — all `{"error": "<string>"}`

Every failure is a bare JSON envelope whose `error` is a **plain string**, not
the object the public API uses:

| Condition | HTTP | Body |
|---|---|---|
| empty/absent `questions` map | 400 | `{"error":"Add at least one question."}` |
| `state` > 12 000 chars | 400 | `{"error":"Keep the state under 12,000 characters."}` |
| no state text and no images | 400 | `{"error":"Give the model something to decide about."}` |
| bad/expired `x-session-token` | 401 | `{"error":"Unauthorized"}` |
| malformed question type (array form) | 400 | the validator's own sentence (see bundle `N()`) |
| non-example image URL | 400 | `{"error":"Only the public example images are available right now."}` |

The messages above are the **validator's**, not the model's: they match the
playground's client-side strings character for character, so the route runs
the same validation before it calls Mercury Decide. The gateway therefore must
not re-run those checks (a second copy drifts); it forwards and translates.

## 5. Images

`images` is an array of **base64 data URLs** (`data:image/png;base64,…`), at
most 3, each ≤ 320 000 characters. A tiny 8×8 PNG supplied as a data URL was
rejected with `{"error":"Only the public example images are available right
now."}` — the web route only accepts its own seeded example images, not
arbitrary uploads. The **public API** is the only surface that accepts
arbitrary images (16 per request, 10 MiB each, PNG/JPEG/WebP/GIF data URLs in
`state`/`instructions` parts).

Gateway consequence: `/v1/decisions` carries `images` **verbatim** in the web
shape. Clients that send an arbitrary image get the upstream's own sentence
back through the error envelope (it is a 4xx answer, not a gateway policy).
Pass-through is the honest contract; the gateway does not vet or rewrite
image URLs.

## 6. Streaming vs whole body, and the public API's differences

| | web `/api/decide/run` | public `POST /v1/decisions` |
|---|---|---|
| transport | one JSON body | one JSON body |
| streaming | none | none ("Streaming is not supported") |
| `model` in request | absent | required (`mercury-decide`) |
| `model` in response | absent | present |
| `state` | string or chat-style parts array | string, object, or array |
| `questions` | **array** of `{id,name,type,instructions,options}` | **map** of name → `{type,instructions,criteria}` |
| `noul` answer | `{"type":"noul","p":0.99}` | `{"type":"noul","noul":0.97}` |
| `choice` answer | `choice`+`probabilities`+`confidence` | same names |
| `score` answer | `score`+`levels[{label,p}]`+`confidence` | `score`+`legend`+`probabilities`+`confidence` |
| `extract` | not offered | fourth question type (`spans`,`not_stated`) |
| usage | `inputTokens`, `latencyMs` | `usage:{input_tokens,output_tokens}` |
| errors | `{"error":"<sentence>"}` | `{"error":{message,type,param,code}}` or `{"detail":[…]}` |
| images | example images only | arbitrary data URLs, 16 × 10 MiB |

**Directive check.** The task says to align to the **upstream web format
as-is** and translate only auth + error envelope. So `/v1/decisions` takes and
returns the web shapes (`questions` array, `answers[].p`/`levels`, top-level
`inputTokens`/`latencyMs`) with no field renaming. The official public API's
map/criteria shape and `mercury-decide` model field are documented here as the
*difference*, not adopted: the gateway rides the same anonymous web transport
as every other route and has no `mercury-decide` API key. Since the upstream
is non-streaming there is no streaming variant to offer, per the task's rule.

## 7. What the probes did **not** establish

- Arbitrary uploads on the web route: proven rejected only for a tiny PNG;
  the error message strongly implies all non-example images are rejected.
- `extract` questions on the web route: the playground does not offer them;
  not probed (would have cost a probe with no design consequence).
- A multi-question request above 8 questions, duplicate names, or an
  option-count breach: the validator sentences are visible in the bundle and
  mirrored server-side; the gateway forwards rather than re-implements them.
