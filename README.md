# inceptionlabs2api

**把 [Inception Chat](https://chat.inceptionlabs.ai)（Mercury 3 扩散式 LLM）封装成标准 OpenAI Chat Completions API 的极简 Go 网关。**

一个二进制、一个模型、零外部存储。匿名会话池自动续期，流式响应按 OpenAI 协议逐帧输出。

[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?style=flat-square&logo=go)](https://go.dev)
[![OpenAI-Compatible](https://img.shields.io/badge/OpenAI-Compatible-412991?style=flat-square&logo=openai)](https://platform.openai.com/docs/api-reference/chat)
[![Docker](https://img.shields.io/badge/Docker-distroless-2496ED?style=flat-square&logo=docker)](Dockerfile)
[![Streaming](https://img.shields.io/badge/Streaming-SSE-2ea44f?style=flat-square&logo=serverless)](docs/SPEC.md)
[![License: Non-Commercial](https://img.shields.io/badge/License-Non--Commercial-red?style=flat-square)](LICENSE)

---

## 简介

Inception Chat 的网页端使用匿名会话：`GET /api/session` 直接签发一个不绑定 IP 的
token，聊天请求带上 `x-session-token` 即可。本项目把这条链路原样桥接出来：

- **匿名会话池**：min 2 / max 8，TTL 30 分钟到期销毁重建，401/403 丢弃换新并最多重试 2 次。
  客户端断连、超时、上游 5xx 都**不会**污染 token。
- **标准库 HTTP/2 + `_vcrcs` 通关（轮换环）**：上游的 Vercel 安全网关要求
  「清除凭证 cookie + 求解时同一个 Chrome UA + HTTP/2」三者同时成立。网关在
  **启动时**即后台本地求解 v2 工作量证明（3 轮 SHA-256 前缀链，约 0.1 秒 CPU，
  无外部依赖），默认保持 **20 个** cookie 的清关环（`IL2API_CLEARANCE_POOL`）：
  启动时只同步填**第 1 个**槽位（约 0.1–0.4 秒）就开始服务，其余槽位由**唯一的
  一个后台 goroutine 顺序**补齐（每次间隔 1–2 秒 + 抖动，约 30–60 秒填满），
  `/healthz` 的 `clearance.live/pool` 与每槽 `state` 可直接观察填充进度；
  每个 cookie 从获取起算、45 分钟主动更换一次（循环一次只换一个槽）；
  请求在槽位间**按请求轮询**；某个请求被 429 挑战时只退役它用过的那一个槽位、
  **立即换下一个活槽位重放**（请求路径永不等待解题），补位由同一个后台循环完成。全部走标准库
  `crypto/tls` + `ForceAttemptHTTP2`，支持 HTTP CONNECT 代理（`HTTPS_PROXY`
  风格），清关与业务请求共用同一出口。
  （此前的 utls + ALPN `http/1.1` 方案已被证明是反的：强制 h1.1 恰恰会让
  cookie 永远失效，整段实现已删除。）
- **零第三方依赖**：`go.mod` 没有 require，`go.sum` 已移除，单一静态二进制。
- **完整的 OpenAI 协议对齐**：
  - 流式：role 首帧 → `reasoning_content` 增量 → `content` 增量 → `finish_reason:"stop"` → `[DONE]`；
  - 非流式：聚合为 `chat.completion`，`message.reasoning_content` 与 `content` 同时返回；
  - 推理通道来自上游 `reasoning-*` 事件（DeepSeek 约定），在正文之前下发——
    这正是审计中 17.5 秒零字节 TTFT 的修复点；
  - 多段流（`reasoning-0 → text-0 → reasoning-1 → text-1`）全程保留；
  - `text-start` 携带的整段快照文本按一个 content delta 输出，不丢字；
  - 错误统一为 `{"error":{"message","type","code"}}`。
- **诚实的边界**：上游不提供 usage，因此响应里**没有** `usage` 字段（不编造估算值）。
- **透传优先，不做自作主张的拦截**：网关只拒绝三类请求——(a) JSON 无法解析、
  (b) 结构上无法表达（缺 `model`/`messages`、消息里没有任何可渲染文本、JSON 类型错误）、
  (c) 鉴权失败。其余一律交给上游：
  - 未知 OpenAI 参数（`temperature`、`top_p`、`n`、`logprobs`、`response_format`、
    `seed`、`user`……）**静默丢弃**，不返回 400——OpenAI SDK 每次都会带这些字段；
  - `tools` / `tool_choice` / `functions` / `function_call` 被丢弃，请求按普通聊天继续；
  - 图片/音频/文件等非文本内容块不再 400，而是转成 `[image]` / `[audio]` /
    `[file]` 这样的短文本占位符，让模型知道有附件；
  - `role:"tool"` / 未知 role 统一按 `user` 处理；
  - **没有** 25 万字符上下文守卫：超长输入直接转给上游，由上游按自己的上限截断；
  - `reasoning_effort` 只把 `minimal` 映射成 `low`，其余原样透传；
  - 上游的非 200 错误正文（以及搜索流里的终止 `error`）**原样翻译**进
    `{"error":{...}}` 信封，只有 HTML 挑战页不会被回显。
- **滑动空闲看门狗**：上游 60 秒没有任何事件才判超时（每行事件都会重置），
  不是按总耗时砍断长回答；流中断后必定补发 error 帧 + `[DONE]`，绝不裸关闭。
- **可选 API Key**：`IL2API_API_KEY` 为空即开放访问；`OPTIONS` 预检与 CORS 头已处理。
- **独立联网搜索**：`POST /v1/web_search` 对接上游 `/search` 实验台的检索流水线
  （`/api/search-lab/run` 的 NDJSON 流），返回重写后的查询与实际命中的来源，
  响应结构与 [simple-chat](https://github.com/Sliverkiss/simple-chat) 完全一致；
  `/healthz` 用 `search.available` 报告上游是否开放检索。
- **联网引用注解**：聊天默认开启上游联网搜索，`source-url` 事件会转成 OpenAI
  `url_citation` 注解——流式随 `finish_reason:"stop"` 帧的 `delta.annotations`
  下发，非流式放在 `message.annotations`；正文里的 `[13]` 不再是没有出处的孤儿标记。
  `index` 按来源到达顺序 1..N（上游不提供索引字段），`start_index`/`end_index`
  在没有偏移信息时按约定为 0；`__searching__` 静默忽略，`__search_error__` 只记日志、
  不中断回答。

范围之外（刻意不做）：usage/token 统计、工具调用、搜索答案正文（只桥接来源）、
管理面板。设计细节见 [docs/SPEC.md](docs/SPEC.md)。

## 快速开始

### Docker（推荐）

```bash
docker compose up -d --build     # 监听 :3000
```

### 本地运行

```bash
cd app
go run .                         # IL2API_ADDR 默认 :3000
```

### 调用

```bash
# 非流式
curl http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"mercury-3","messages":[{"role":"user","content":"say hi"}]}'

# 流式（reasoning_content 在 content 之前）
curl -N http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"mercury-3","stream":true,
       "messages":[{"role":"user","content":"用一句话解释扩散模型"}]}'
```

任何 OpenAI 客户端把 base URL 指向 `http://host:3000/v1`、模型名填 `mercury-3` 即可。

> **推理强度**：可传 `"reasoning_effort": "low" | "medium" | "high"`（默认 `medium`），
> 映射到上游的 `reasoningEffort`。简单问题用 `low` 响应更快。

## 配置

全部通过环境变量（前缀 `IL2API_`），非法值会在启动时直接报错。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `IL2API_ADDR` | `:3000` | 监听地址（完整 `host:port`，不是裸端口） |
| `IL2API_API_KEY` | 空 | `/v1/*` 的 Bearer Key；空 = 不鉴权。`/healthz` 永远免鉴权 |
| `IL2API_UPSTREAM_BASE` | `https://chat.inceptionlabs.ai` | 上游地址 |
| `IL2API_MODEL` | `mercury-3` | 默认/主模型名 |
| `IL2API_MODELS` | 同 `IL2API_MODEL` | 逗号分隔的可接受模型列表（`/v1/models` 返回） |
| `IL2API_PROXY` | 回退 `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` | 出站 HTTP CONNECT 代理，支持 `http://host:port`、`host:port`、`https://`；可带 `user:pass@`（以 Basic 方式发送，日志与错误里会脱敏） |
| `IL2API_POOL_MIN` | `2` | 会话池最小 token 数 |
| `IL2API_POOL_MAX` | `8` | 会话池最大 token 数 |
| `IL2API_POOL_TTL` | `30m` | token 寿命，到期销毁重建 |
| `IL2API_IDLE_TIMEOUT` | `60s` | 滑动空闲看门狗（每个上游事件都会重置） |
| `IL2API_CONTEXT_CHARS` | `0` | 已废弃的空操作，仅为配置兼容保留；不再有输入长度守卫 |
| `IL2API_CLEARANCE_REFRESH` | `45m` | 每个 `_vcrcs` 槽位的主动刷新年龄（cookie 实际有效期 60 分钟） |
| `IL2API_CLEARANCE_COOLDOWN` | `15s` | 单个槽位清关**失败**后的最小重试间隔，避免挑战风暴（成功求解不会开启窗口，429 重放不影响其它槽位） |
| `IL2API_CLEARANCE_POOL` | `20` | 清关 cookie 轮换环大小；启动只同步填第 1 槽，其余由后台循环顺序补齐；`1` 即回到单 cookie 行为 |
| `IL2API_SESSION_TIMEOUT` | `15s` | 单次 `/api/session` 请求超时 |
| `IL2API_SEARCH_TIMEOUT` | `60s` | `/v1/web_search` 的滑动空闲看门狗（每个 NDJSON 行都会重置） |
| `IL2API_SEARCH_CONFIG_CHECK` | `on` | `on`/`off`，是否在启动时后台探测一次 `/api/search-lab/config`；`off` 时 `search.available` 恒为 `null` |
| `IL2API_SEARCH_PROBE_TIMEOUT` | `15s` | 单次启动探测的超时上限 |
| `IL2API_AUDIO_TIMEOUT` | `120s` | `/api/tts`、`/api/stt` 单次调用的整体超时（响应头 + 响应体） |
| `IL2API_AUDIO_MAX_BYTES` | `26214400`（25 MiB） | `/v1/audio/transcriptions` 上传上限，超出直接 413，不调用上游 |
| `IL2API_DECISIONS_TIMEOUT` | `120s` | `/api/decide/run` 单次调用的整体超时（响应头 + 响应体） |

完整注释版示例见 [`app/.env.example`](app/.env.example)。

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/v1/chat/completions` | OpenAI 兼容，`stream` 可选 |
| `POST` | `/v1/web_search` | 独立联网搜索，返回查询与来源（见下） |
| `POST` | `/v1/audio/speech` | OpenAI 语音合成，返回 `audio/mpeg`（见下） |
| `POST` | `/v1/audio/transcriptions` | OpenAI 语音转写，multipart 上传（见下） |
| `POST` | `/v1/decisions` | Mercury Decide 结构化判断：按上游网页原格式透传（见下） |
| `GET` | `/v1/models` | 返回配置的模型列表 |
| `OPTIONS` | `/v1/*` | CORS 预检（返回 204） |
| `GET` | `/healthz` | 存活探针 + 会话池大小 + 清关状态 + 请求计数 |

### 请求字段与校验边界

真正被使用的字段是 `model`、`messages`、`stream`、`reasoning_effort`；**其余顶层字段
（`temperature`、`top_p`、`n`、`stop`、`response_format`、`logprobs`、`stream_options`、
`user`、`seed`……）一律静默丢弃，不报错**。`tools` / `tool_choice` / `functions` /
`function_call` 同样丢弃，请求按普通聊天继续。消息里的 `image_url`、`input_audio`、
`file` 等非文本块会被渲染成简短的文本占位符。`role: "developer"` 映射为 `system`，
`role: "tool"` 与未知 role 按 `user` 处理。

网关只在以下情况返回 400/401：JSON 无法解析或尾部有多余内容、`model`/`messages`
缺失或为空、某条消息没有任何可渲染文本、字段 JSON 类型错误、API Key 错误。
其余判断权归上游；上游的错误正文会翻译进 OpenAI 错误信封。

### 流式响应示例

```
data: {"id":"chatcmpl-9f2c1a7d3b40","object":"chat.completion.chunk","created":1792556641,"model":"mercury-3","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null,"logprobs":null}]}

data: {"id":"chatcmpl-9f2c1a7d3b40",...,"choices":[{"index":0,"delta":{"reasoning_content":"Analyzing the question.\n"},"finish_reason":null,"logprobs":null}]}

data: {"id":"chatcmpl-9f2c1a7d3b40",...,"choices":[{"index":0,"delta":{"content":"扩散模型"},"finish_reason":null,"logprobs":null}]}

data: {"id":"chatcmpl-9f2c1a7d3b40",...,"choices":[{"index":0,"delta":{"annotations":[{"type":"url_citation","index":1,"url":"https://…","title":"…","start_index":0,"end_index":0}]},"finish_reason":"stop","logprobs":null}]}

data: [DONE]
```

非流式响应：

```json
{
  "id": "chatcmpl-9f2c1a7d3b40",
  "object": "chat.completion",
  "created": 1792556641,
  "model": "mercury-3",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "……", "reasoning_content": "……",
                "annotations": [{"type": "url_citation", "index": 1, "url": "https://…",
                                 "title": "…", "start_index": 0, "end_index": 0}]},
    "finish_reason": "stop",
    "logprobs": null
  }]
}
```

### 独立联网搜索 `POST /v1/web_search`

上游 `/search` 实验台背后是一条独立的检索流水线：`POST /api/search-lab/run`
以 NDJSON 逐行推送 `plan → retrieve → rerank → followup → synthesize` 各阶段，
其中 `docs` 事件携带按相关度排序的 `sources:[{title,url,score}]`。本接口把这条流
桥接成 [simple-chat](https://github.com/Sliverkiss/simple-chat) 的 `web_search`
响应形状，客户端换 base URL 即可复用同一套解析：

```bash
curl http://localhost:3000/v1/web_search \
  -H "Content-Type: application/json" \
  -d '{"query":"最近有哪些国家试行四天工作制？",
       "config":{"fanout":3,"depth":1,"keep_top_k":10,"effort":"instant"}}'
```

```jsonc
{"object": "web_search",
 "queries": ["four-day workweek trial results countries"],   // 上游重写出的实际查询
 "results": [
   {"url": "https://www.aph.gov.au/...", "title": "Four-day work week – Parliament of Australia",
    "snippet": "", "cite_index": 1},
   {"url": "https://www.4dayweek.com/research", "title": "4 Day Week Research Reports", "cite_index": 2}
 ]}
```

- `config` 可省略：`fanout` 默认 3、`depth` 默认 1、`keep_top_k` 默认 10、`effort`
  默认 `instant`；只有 `fanout`/`depth` 按上游公布的范围（1–5 / 1–3）收敛——
  这两个范围是上游 `/api/search-lab/config` 自己声明的。`keep_top_k` **不收敛**
  （上游没公布范围，收敛到 1–50 是网关自己编的数），`effort` 原样透传。
- `queries` 收集自所有 `queries` 事件（含多轮 follow-up），去重后按出现顺序返回。
- `results` 是最终一轮 `docs` 的名称/链接/名次，`cite_index` 为 1..N；
  **`snippet` 恒为空**——上游 NDJSON 里根本没有逐条摘要，本网关不编造。
- 读到最后一轮 `docs` 就停止，不等待 `synthesize`：答案正文不属于本接口，
  提前停止可省下数秒与多次 LLM 调用。
- 只返回绝对的 `http(s)` 链接；`javascript:`、`data:`、相对路径与含控制字符的
  URL 会被丢弃（消费方通常直接把它们渲染成可点击链接）。
- 未知字段（含 `config` 内未知字段）静默丢弃；只有 `fanout`/`depth` 按上游
  公布范围收敛，`keep_top_k` 与 `effort` 原样透传（上游自己判断）。只有 JSON
  损坏、尾部多余内容、查询缺失/非字符串/为空才是 400。
- 上游没有结果时返回 `200` + `"results":[]`，这是诚实的空列表，不重试。
- 响应带 `Cache-Control: no-store`；本接口不流式，整次检索完成后一次性返回。

错误映射与聊天接口一致：会话池为空 `503 upstream_unavailable`；上游非 200
或流内终止 `error` 事件 `502 upstream_error`（**上游自己的错误文本会原样保留**
在信封里）；`docs` 之前流被截断 `502 upstream_stream_error`；滑动空闲窗口
（默认 60 秒，每行重置）超时 `504 upstream_idle_timeout`，整次运行另有 10 个
空闲窗口的绝对上限。

> **配置探测只供参考。** 启动时对 `/api/search-lab/config` 的探测只会写入
> `/healthz` 的 `search.available`，**从不拦截请求**：即使上游明确报告
> `retrievalAvailable:false`（或探测结果已经过期/被挑战页误读），请求照样发往
> 上游，客户端看到的是上游自己的回答或错误。探测请求会带上清关 cookie 与会话
> token；如果响应里根本没有 `retrievalAvailable` 字段——例如又一个挑战 JSON——
> `search.available` 会保持 `null`，绝不会被误读成 `false`。

> 上游对 NDJSON 默认做 Brotli 压缩。网关发送 `Accept-Encoding: identity`，
> 并用标准库解码 `gzip`；本项目零第三方依赖，没有 Brotli 解码器，因此若中间层
> 仍强行返回 `br`，会得到一个**明确指出编码**的错误，而不是乱码或静默空结果。

### 语音合成 `POST /v1/audio/speech`

```bash
curl http://localhost:3000/v1/audio/speech \
  -H "Content-Type: application/json" \
  -d '{"model":"tts-1","input":"你好，这是一段语音测试。","voice":"alloy"}' \
  -o speech.mp3
```

| 字段 | 行为 |
|---|---|
| `input` | **必填**：缺失、非字符串或全空白 → 400（`param:"input"`）。**没有长度上限**，原样交给上游（上游网页端自己截到 5 000 字符） |
| `voice` | 大小写不敏感、两端空白会被去掉。OpenAI 的六个标准名会被**映射**到上游音色：`alloy→sarah`、`echo→chris`、`fable→charlie`、`onyx→chris`、`nova→jessica`、`shimmer→jessica`。**其它任何名字都原样转发**（小写化后），由上游决定如何回答；留空则用上游默认。上游**不校验**音色名：一个它不认识的音色会返回 `200` 加一段约 0.44 秒的短音频，而不是报错——这是上游的行为，网关不替它拒绝 |
| `response_format` | 接受但忽略：上游只产 MP3，响应**永远是** `audio/mpeg`。要 wav 请客户端自己转 |
| `speed` | 静默丢弃（上游没有语速参数） |
| `model` | 接受但忽略（上游只有一个语音引擎） |

响应是**完整**的 MP3（不是分块伪流），带 `Cache-Control: no-store`。网关会把上游
音频读完再提交响应头，因此上游失败时客户端收到的是标准的 JSON 错误信封，而不是
半截音频。

音色清单来自 bundle 考古（网页端从不下发音色数组，只有场景配置里的
`sarah`/`chris`/`charlie`/`jessica`）加上 3 次实测确认：同样文本下三种音色的音频
字节与时长各不相同（chris 2.09 s / charlie 2.22 s / sarah 2.27 s）。
所以想拿到“真正的语音”，请用上面六个 OpenAI 名字之一或那四个上游名；
写错名字不会得到 400，只会得到一段很短的、可能不符合预期的音频。

`/v1/audio/transcriptions` 的 25 MiB 上限是**网关自己的**保护（上传要整个读进
内存；按实测约 12 秒/MB 的上游处理速度，25 MB 也已经超过 120 秒调用超时），
不是上游的限额：实测中上游接受了 2.5 MB 的音频并用了约 31 秒返回文本。

### 语音转写 `POST /v1/audio/transcriptions`

```bash
curl http://localhost:3000/v1/audio/transcriptions \
  -F file=@speech.mp3 -F model=whisper-1
# {"object":"transcription","text":"你好，这是一段语音测试。"}
```

> **MP3 专用合约。** 上游 `/api/stt` 只解码真正的 MPEG 音频；WAV（8/16/48 kHz）、
> WebM 等一律返回 `200 {"text":""}`（静默空结果，不是报错）。网关**不做转码**、
> 不引入 ffmpeg —— 零第三方依赖优先。上传的字节按原样转发；网关只做一件事：
> 嗅探开头是不是 `ID3` 或合法 MPEG 帧头，据此把 multipart 分片的
> `Content-Type` 标成 `audio/mpeg`，否则标成 `application/octet-stream`。
> 因此**需要转写 WAV/WebM/M4A 的客户端，请在本网关之前自行转成 MP3**。
> 空文本会原样返回（`{"text":""}`），不会被网关改写成一个错误。

- `file` 必填；缺失、为空或无法解析 → 400（`param:"file"`）。
- `model` / `language` / `prompt` / `temperature` / `response_format` 等字段静默丢弃
  （上游不接受这些参数）；响应恒为 OpenAI `json` 形状。
- 上传上限 `IL2API_AUDIO_MAX_BYTES`（默认 25 MiB），超出 413 且不触达上游。
- 上游的 `Invalid file type` 等错误会原样翻译为 `502 upstream_error`。

`/healthz` 的 `counters` 新增 `tts` 与 `stt`，分别统计两个音频路由承接的请求数。

### 结构化判断 `POST /v1/decisions`

对接上游 Decide 实验台的 `/api/decide/run`（Mercury Decide，专用分类模型，不是 Mercury 3
聊天）。**请求与响应都按上游网页格式原样对齐**，网关只翻译鉴权与错误信封：

```bash
curl http://localhost:3000/v1/decisions \
  -H "Content-Type: application/json" \
  -d '{"state":"客户被重复扣款两次，要求退款。",
       "questions":[{"id":"q-1","name":"wants_refund","type":"noul",
                     "instructions":"客户是在要求退款吗？","options":[]}],
       "images":[]}'
# {"answers":{"wants_refund":{"type":"noul","p":0.999}},"inputTokens":72,"latencyMs":226}
```

- `questions` 是**数组**（`{id,name,type,instructions,options}`），不是官方公网 API 的
  映射表 —— 网页路由只接受数组，实测映射表会被它自己的校验器拒绝。
- `type` 支持 `noul`（是/否）、`choice`（`options:[{id,name,description}]`）、
  `score`（`options:[{id,name:"",description}]`，按顺序的等级）。
- 响应里 `noul` 答 `p`；`choice` 答 `choice` + `probabilities` + `confidence`；
  `score` 答**小数索引** `score` + `levels[{label,p}]` + `confidence`。
  顶层 `inputTokens`/`latencyMs`，没有 `model`、没有 `usage`。
- 上游**不流式**：一次 POST、一个完整 JSON。因此网关也只有非流式。
- `images` 传 base64 data URL；网页路由只认它自带的示例图，其它图片会返回
  `400 {"error":"Only the public example images are available right now."}` ——
  那是上游自己的回答，网关原样透传。
- 上游的校验 4xx（空 questions、state 超 12 000 字符等）按项目统一规则翻译为
  `502 upstream_error`，正文保留上游原句（例如 `Add at least one question.`）。
- 整体超时 `IL2API_DECISIONS_TIMEOUT`（默认 120s），与音频路由共用同一套
  whole-call 截止机制；请求体沿用 6 MiB JSON 读取器。

`/healthz` 的 `counters` 新增 `decisions`。

### 错误码

| HTTP | code | 场景 |
|---|---|---|
| 400 | `invalid_request_error` | JSON 损坏/尾部多余内容、缺 model/messages、消息无文本、类型错误 |
| 401 | `invalid_api_key` | 缺少或错误的 Bearer Key |
| 404 | `model_not_found` | 模型不在 `IL2API_MODELS` |
| 502 | `upstream_error` / `upstream_stream_error` / `upstream_auth_failed` | 上游异常或流被截断（正文来自上游） |
| 503 | `upstream_clearance_failed` | 上游挑战无法完成（PoW 版本变化、出口/UA 被拒） |
| 503 | `upstream_unavailable` | 会话池为空且不是清关问题 |
| 504 | `upstream_idle_timeout` | 上游超过空闲窗口没有任何事件 |
| 413 | `request_too_large` | `/v1/audio/transcriptions` 上传超过 `IL2API_AUDIO_MAX_BYTES` |

流式响应一旦发出头部就无法改状态码：中途失败会改为发送一个
`{"error":{...}}` SSE 帧，随后补 `data: [DONE]`。

## 部署

### belmo（Nixpacks，端口 3000）

belmo 的部署引擎总会扫描仓库的 **`app/` 子目录**，这正是 Go 源码放在 `app/` 的原因。

1. 在 belmo 导入本仓库，构建方式选 **Nixpacks**。
2. **Base Directory 填 `app`** —— 留空会导致检测不到 Go 项目。
3. Build / Start 命令留空，`app/nixpacks.toml` 已固定为 `go build -o app .` / `./app`。
4. **端口填 `3000`**，环境变量设 `IL2API_ADDR=:3000`（注意带冒号）。
5. 环境变量至少配置 `IL2API_API_KEY`；其余按需。

### Docker / docker compose

```bash
docker compose up -d --build      # 3000:3000，distroless nonroot (uid 65532)
```

镜像为 `golang:1.24-alpine` 构建 → `gcr.io/distroless/static-debian12:nonroot` 运行，
静态二进制、无 shell、零第三方 Go 依赖。

> **出口 IP 与清关。** `_vcrcs` cookie 绑定「出口 IP + UA + HTTP/2」，所以网关
> 会从它实际使用的出口自行求解，无需外部刷新服务。若代理配置不正确，登录页
> 仍会挑战：服务照常启动，但 `/healthz` 的 `clearance.has_cookie` 为 `false`、
> `pool.size` 保持 0，聊天返回 `503 upstream_clearance_failed` 或
> `503 upstream_unavailable`，日志里会看到
> `upstream: checkpoint challenged ... retiring the slot and replaying once`。
> 请给容器设置 `IL2API_PROXY` 并把清关与业务流量放在**同一个**代理出口上
> （例如 `IL2API_PROXY=http://10.0.0.2:3128`）。**部署环境的出口与本地探测环境
> 可能不同**，请以部署后 `/healthz` 的 `clearance` 与 `pool.size` 为准。

### 观测

```bash
curl -s http://localhost:3000/healthz | jq
# {"ok":true,"pool":{"size":2,"min":2,"max":8,"expired":0},
#  "search":{"available":true},
#  "clearance":{"live":20,"pool":20,"has_cookie":true,"age_seconds":312,
#               "refresh_count":20,
#               "slots":[{"cookie":true,"state":"live","age_seconds":312,"refresh_count":1},
#                        {"cookie":true,"state":"live","age_seconds":104,"refresh_count":1},
#                        {"cookie":false,"state":"pending","age_seconds":0,"refresh_count":0}]},
#  "counters":{"requests":12,"streams":9,"errors":0,"upstream_errors":0,"tts":0,"stt":0}}
```

## 开发

```bash
cd app
go build ./... && go vet ./... && go test ./...        # 每个提交都必须是绿的
go test ./... -race -cover                              # 竞态 + 覆盖率（约 92%）
```

测试策略：全部针对 `httptest` / 本地 TLS 监听器上的模拟上游，不访问真实的
Vercel（本次的线上探测只有 6 次清关环证据 + 1 次 2.5 MB 上传，外加最后的 3 次
冒烟）。PoW 求解器用真实 Chromium 抓取的 token/解做回归；清关管理器用模拟挑战端点
覆盖获取、cookie 存储、45 分钟主动刷新、冷却、429 重新获取与 403 拒绝；传输层用
本地 h2 服务端证明协商出 HTTP/2，并用本地 CONNECT 代理验证出口。搜索链路由模拟的
NDJSON 上游覆盖：完整流水线、多轮 follow-up、gzip 编码（`br` 为带类型的错误）、
缺失 `docs`、终止 `error`、流截断、非 200、`retrievalAvailable:false`、空闲超时、
启动探测（成功/失败/关闭）以及恶意来源清洗。仓库根目录的 `Makefile` 提供
`make test` / `make race` / `make cover` / `make docker`。

## 许可

本项目采用 [CC BY-NC-SA 4.0](LICENSE)（署名—非商业性使用—相同方式共享）许可：
允许非商业用途的复制、修改与分发，需保留署名并以相同许可共享。商业使用请联系作者。
