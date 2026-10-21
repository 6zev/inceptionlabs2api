# inceptionlabs2api

**把 [Inception Chat](https://chat.inceptionlabs.ai)（Mercury 3 扩散式 LLM）封装成标准 OpenAI Chat Completions API 的极简 Go 网关。**

一个二进制、一个模型、零外部存储。匿名会话池自动续期，流式响应按 OpenAI 协议逐帧输出。

[![Go](https://img.shields.io/badge/Go-1.23-00ADD8?style=flat-square&logo=go)](https://go.dev)
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
- **utls Chrome 指纹 + ALPN 仅 `http/1.1`**：Vercel 风控会按 HTTP/2 JA4 拦截，
  所以每个请求都用全新 ClientHello，并支持 HTTP CONNECT 代理（`HTTPS_PROXY` 风格）。
- **完整的 OpenAI 协议对齐**：
  - 流式：role 首帧 → `reasoning_content` 增量 → `content` 增量 → `finish_reason:"stop"` → `[DONE]`；
  - 非流式：聚合为 `chat.completion`，`message.reasoning_content` 与 `content` 同时返回；
  - 推理通道来自上游 `reasoning-*` 事件（DeepSeek 约定），在正文之前下发——
    这正是审计中 17.5 秒零字节 TTFT 的修复点；
  - 多段流（`reasoning-0 → text-0 → reasoning-1 → text-1`）全程保留；
  - `text-start` 携带的整段快照文本按一个 content delta 输出，不丢字；
  - 错误统一为 `{"error":{"message","type","code"}}`。
- **诚实的边界**：上游不提供 usage，因此响应里**没有** `usage` 字段（不编造估算值）；
  不支持的 OpenAI 参数（`temperature`、`top_p`、`n`、`logprobs`、`response_format`……）
  一律 **400 并列出全部参数名**，而不是静默忽略；图片/音频/工具调用同样显式 400。
- **上下文守卫**：输入超过 25 万字符（按 Unicode 码点计）直接 400
  `context_length_exceeded`，绝不静默截断。
- **滑动空闲看门狗**：上游 60 秒没有任何事件才判超时（每行事件都会重置），
  不是按总耗时砍断长回答；流中断后必定补发 error 帧 + `[DONE]`，绝不裸关闭。
- **可选 API Key**：`IL2API_API_KEY` 为空即开放访问；`OPTIONS` 预检与 CORS 头已处理。

范围之外（刻意不做）：usage/token 统计、工具调用、Web 搜索、管理面板。
设计细节见 [docs/SPEC.md](docs/SPEC.md)。

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
| `IL2API_PROXY` | 回退 `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` | 出站 HTTP CONNECT 代理，支持 `http://host:port`、`host:port`、`https://` |
| `IL2API_POOL_MIN` | `2` | 会话池最小 token 数 |
| `IL2API_POOL_MAX` | `8` | 会话池最大 token 数 |
| `IL2API_POOL_TTL` | `30m` | token 寿命，到期销毁重建 |
| `IL2API_IDLE_TIMEOUT` | `60s` | 滑动空闲看门狗（每个上游事件都会重置） |
| `IL2API_CONTEXT_CHARS` | `250000` | 输入字符上限；`-1` 关闭守卫 |
| `IL2API_SESSION_TIMEOUT` | `15s` | 单次 `/api/session` 请求超时 |

完整注释版示例见 [`app/.env.example`](app/.env.example)。

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/v1/chat/completions` | OpenAI 兼容，`stream` 可选 |
| `GET` | `/v1/models` | 返回配置的模型列表 |
| `GET` | `/healthz` | 存活探针 + 会话池大小 + 请求计数 |

### 请求字段

只接受 `model`、`messages`、`stream`、`reasoning_effort`。其他顶层字段（`temperature`、
`top_p`、`n`、`stop`、`response_format`、`logprobs`、`stream_options`、`user`……）
返回 400 `unsupported_parameter`，错误信息会**逐一列出**不支持的字段名和允许的字段集合。
`tools` / `tool_choice` / `functions` / `function_call` 返回 400 `tool_calling_unsupported`。
消息数组里的 `image_url`、`input_audio`、`file` 等非文本块返回 400
`content_part_unsupported`。`role: "developer"` 会映射为 `system`。

### 流式响应示例

```
data: {"id":"chatcmpl-9f2c1a7d3b40","object":"chat.completion.chunk","created":1792556641,"model":"mercury-3","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null,"logprobs":null}]}

data: {"id":"chatcmpl-9f2c1a7d3b40",...,"choices":[{"index":0,"delta":{"reasoning_content":"Analyzing the question.\n"},"finish_reason":null,"logprobs":null}]}

data: {"id":"chatcmpl-9f2c1a7d3b40",...,"choices":[{"index":0,"delta":{"content":"扩散模型"},"finish_reason":null,"logprobs":null}]}

data: {"id":"chatcmpl-9f2c1a7d3b40",...,"choices":[{"index":0,"delta":{},"finish_reason":"stop","logprobs":null}]}

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
    "message": {"role": "assistant", "content": "……", "reasoning_content": "……"},
    "finish_reason": "stop",
    "logprobs": null
  }]
}
```

### 错误码

| HTTP | code | 场景 |
|---|---|---|
| 400 | `invalid_request_error` | JSON 损坏、字段值非法、空消息 |
| 400 | `unsupported_parameter` | 不支持的 OpenAI 参数（信息里列出全部） |
| 400 | `content_part_unsupported` | 图片/音频/文件内容块 |
| 400 | `tool_calling_unsupported` | 工具调用相关字段或消息 |
| 400 | `context_length_exceeded` | 输入超过 `IL2API_CONTEXT_CHARS` |
| 401 | `invalid_api_key` | 缺少或错误的 Bearer Key |
| 404 | `model_not_found` | 模型不在 `IL2API_MODELS` |
| 502 | `upstream_error` / `upstream_stream_error` / `upstream_auth_failed` | 上游异常或流被截断 |
| 503 | `upstream_unavailable` | 会话池为空（通常是出口 IP 被 Vercel 挑战） |
| 504 | `upstream_idle_timeout` | 上游超过空闲窗口没有任何事件 |

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

镜像为 `golang:1.23-alpine` 构建 → `gcr.io/distroless/static-debian12:nonroot` 运行，
静态二进制、无 shell、无外部依赖。

> **出口 IP 很重要。** 上游的 Vercel 风控按 IP 挑战。如果部署机被挑战，
> 服务仍能启动（不会崩溃），但 `/healthz` 的 `pool.size` 会一直是 0，
> 聊天接口返回 `503 upstream_unavailable`，日志里会出现
> `pool: refill failed ... status 429 ... Vercel Security Checkpoint`。
> 此时给容器设置 `IL2API_PROXY` 走一个未被挑战的出口即可，例如
> `IL2API_PROXY=http://10.0.0.2:3128`。**部署环境的出口与本地探测环境可能不同**，
> 请以部署后 `/healthz` 的 `pool.size` 为准。

### 观测

```bash
curl -s http://localhost:3000/healthz | jq
# {"ok":true,"pool":{"size":2,"min":2,"max":8,"expired":0},
#  "counters":{"requests":12,"streams":9,"errors":0,"upstream_errors":0}}
```

## 开发

```bash
cd app
go build ./... && go vet ./... && go test ./...        # 每个提交都必须是绿的
go test ./... -race -cover                              # 竞态 + 覆盖率（约 88%）
```

测试策略：全部针对 `httptest` / 本地 TLS 监听器上的模拟上游，不访问真实的
Vercel；utls 拨号器只用本地证书与本地 CONNECT 代理验证。仓库根目录的
`Makefile` 提供 `make test` / `make race` / `make cover` / `make docker`。

## 许可

本项目采用 [CC BY-NC-SA 4.0](LICENSE)（署名—非商业性使用—相同方式共享）许可：
允许非商业用途的复制、修改与分发，需保留署名并以相同许可共享。商业使用请联系作者。
