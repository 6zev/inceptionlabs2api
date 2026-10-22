<div align="center">

# inceptionlabs2api

**OpenAI-compatible gateway for Inception's Mercury series**

Diffusion LLM, native tool calling, web search, audio — behind one clean API.

[![License](https://img.shields.io/badge/license-CC%20BY--NC--SA%204.0-blue)](./LICENSE)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Docker](https://img.shields.io/badge/docker-ready-2496ED?logo=docker&logoColor=white)](./Dockerfile)

</div>

## Features

- **OpenAI-compatible** — drop-in `/v1/chat/completions`, works with any OpenAI SDK client
- **Native tool calling** — structured `tool_calls`, streaming, multi-round loops, parallel calls
- **Gateway web search** — the model searches the live web mid-answer through `web_search`, results are fed back invisibly
- **Decisions endpoint** — `/v1/decisions` for structured choice tasks
- **Audio** — `/v1/audio/speech` and `/v1/audio/transcriptions`
- **Session pool** — warmed sessions, automatic rotation, transparent health at `/healthz`

## Quick Start

```bash
git clone https://github.com/Sliverkiss/inceptionlabs2api.git
cd inceptionlabs2api

cp app/.env.example app/.env        # edit: IL2API_API_KEY
make run                            # ... or:
docker build -t inceptionlabs2api . && docker run -p 3000:3000 --env-file app/.env inceptionlabs2api
```

```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer $IL2API_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"mercury-3","messages":[{"role":"user","content":"Hello"}]>'
```

## Endpoints

| Route | Purpose |
|---|---|
| `POST /v1/chat/completions` | Chat, streaming + tool calls + gateway web_search loop |
| `POST /v1/web_search` | Standalone web search (query + optional config) |
| `POST /v1/audio/speech` | Text-to-speech |
| `POST /v1/audio/transcriptions` | Speech-to-text (multipart upload) |
| `POST /v1/decisions` | Structured decision tasks |
| `GET /v1/models` | Model list |
| `GET /healthz` | Liveness + pool state |

### Tool calling

Any OpenAI-style `tools` array works — the model emits standard `tool_calls`,
and a tool result sent back as a `role:"tool"` message continues the loop:

```json
{
  "model": "mercury-3",
  "tools": [{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
  "tool_choice": "auto",
  "messages": [{"role":"user","content":"Weather in Tokyo?"}]
}
```

`web_search` is additionally built into the gateway: when enabled, the model
can search the live web mid-answer and cite results, without any client-side
tool definitions.

## Configuration (key env)

| Variable | Default | Purpose |
|---|---|---|
| `IL2API_API_KEY` | — | Gateway auth bearer |
| `IL2API_ADDR` | `:3000` | Listen address |
| `IL2API_POOL_MIN` / `MAX` | 2 / 8 | Session pool bounds |
| `IL2API_IDLE_TIMEOUT` | 60s | Upstream idle watchdog |
| `IL2API_CONTEXT_CHARS` | 0 | Prompt cap (0 = off) |
| `IL2API_PROXY` | — | Optional HTTP proxy |

See [`app/.env.example`](./app/.env.example) for the full list.

## License

**CC BY-NC-SA 4.0** — non-commercial use with attribution, share-alike. See [`LICENSE`](./LICENSE).
