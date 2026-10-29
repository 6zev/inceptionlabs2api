<div align="center">

# inceptionlabs2api

**Inception Mercury 系列的 OpenAI 兼容网关**

扩散式 LLM · 原生工具调用 · 联网搜索 · 语音 — 一个网关全部搞定。

[![License](https://img.shields.io/badge/license-CC%20BY--NC--SA%204.0-blue)](./LICENSE)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8?logo=go)](https://go.dev)
[![Docker](https://img.shields.io/badge/docker-ready-2496ED?logo=docker)](./Dockerfile)

</div>

## 功能

- **OpenAI 兼容** — 原样接入 `/v1/chat/completions`，任意 OpenAI SDK 直接可用
- **原生工具调用** — 标准 `tool_calls`、流式、多轮回灌、并行调用
- **网关联网搜索** — 模型回答中途自主调用 `web_search` 检索实时信息，结果自动回灌并引用
- **决策接口** — `/v1/decisions` 结构化选择任务
- **语音** — 文字转语音 / 语音转文字
- **会话池** — 预热会话、自动轮换、`/healthz` 透明可观测

## 快速开始

```bash
git clone https://github.com/Sliverkiss/inceptionlabs2api.git
cd inceptionlabs2api

cp app/.env.example app/.env        # 编辑 IL2API_API_KEY
make run                            # 或者:
docker build -t inceptionlabs2api . && docker run -p 3000:3000 --env-file app/.env inceptionlabs2api
```

## 接口一览

| 路由 | 用途 |
|---|---|
| `POST /v1/chat/completions` | 对话（流式 / 工具调用 / 内置联网搜索） |
| `POST /v1/web_search` | 独立联网搜索 |
| `POST /v1/audio/speech` | 文字转语音 |
| `POST /v1/audio/transcriptions` | 语音转文字（multipart 上传） |
| `POST /v1/decisions` | 结构化决策任务（body 原样透传上游） |
| `GET /v1/models` | 模型列表 |
| `GET /healthz` | 存活检查 + 会话池状态 |

所有接口鉴权方式一致：`Authorization: Bearer $IL2API_API_KEY`。

---

## 对话 `/v1/chat/completions`

**请求体**（OpenAI Chat Completions 格式的实用子集；未知字段静默丢弃不报错）：

```json
{
  "model": "mercury-3",
  "messages": [
    {"role": "system", "content": "你是一个有用的助手"},
    {"role": "user", "content": "你好"}
  ],
  "stream": true,
  "reasoning_effort": "instant",
  "tools": [],
  "tool_choice": "auto",
  "parallel_tool_calls": true
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `model` | string | 必填，默认且唯一为 `mercury-3`（见下方说明） |
| `messages` | array | 必填，`system` / `user` / `assistant` / `tool` 轮次历史 |
| `stream` | bool | 默认 `false`；`true` 时返回 SSE 流 |
| `reasoning_effort` | string | 思考强度 `instant` / `low` / `medium` / `high`；**不传 = `medium`**（见下方实测） |
| `tools` | array | OpenAI function 工具定义（见下） |
| `tool_choice` | string/object | `"auto"` / `"required"` / `{"type":"function","function":{"name":"..."}}` |
| `parallel_tool_calls` | bool | 是否允许一轮并行调用多个工具 |

**响应**：OpenAI 标准格式。`stream=false` 返回完整 `chat.completion`；`stream=true` 返回 SSE `data:` 帧，以 `data: [DONE]` 结束。

```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Authorization: Bearer $IL2API_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"mercury-3","messages":[{"role":"user","content":"Hello"}]}'
```

### 关于 `model`：只有一个模型

**上游只有一个模型。** 实测（2026-10-26）上游 `/api/chat` **完全忽略**
`model` 字段：`gpt-4`、`claude-3-opus`、甚至 `totally-fake-model-xyz`
都能得到正常回答（空字符串则报 `model is required`）。不存在按名字切换模型
的机制，也没有可供列举的模型清单接口。

因此网关的 `model` 字段是**标签**而非**选择器**：

- 默认只接受 `mercury-3`，`/v1/models` 也只返回它。
- `IL2API_MODELS` 是**接受名单**，不是模型目录。往里加 `mercury-4`
  只会让网关多接受一个名字，后端仍是同一个模型——不解决任何问题，
  反而让客户端误以为存在多个模型可选。
- 判断"是否能用某个模型"的正确依据是上游，而不是这个名单。

**没有 `-thinking` 别名**：网关不再自动派生 `<名字>-thinking`。那个别名只是
同一后端的第二个标签，`/v1/models` 已经用 `capabilities.reasoning` /
`supports_reasoning` / `reasoning_levels` 明确告知"这个模型会思考"，
客户端应当读 capability 而不是猜 ID。请求 `mercury-3-thinking` 会得到
`model_not_found`，与任何其它未列出的名字一致。

> Cherry Studio 等**只按 ID 猜**的客户端（`REASONING_REGEX` 匹配
> `reasoning` / `reasoner` / `thinking` / `think`）因此不会显示思考强度
> 选项、也不会发送 `reasoning_effort` —— 网关仍会正常返回回答，只是拿不到
> `reasoning_content`。需要时把 `IL2API_MODEL` 设成含该词的名字
> （如 `mercury-3-thinking`）即可，那是你自己的显式命名，不是网关派生。

### `reasoning_effort` 实测：实际只有三档

**结论先说：日常用 `medium`（也就是不传）；`low` 适合要快又要对的场景；
`instant` 只适合闲聊；`high` 相比 `medium` 没有可测收益，不建议用。**

网关把这个字段原样透传给上游（仅 `minimal`→`low` 做映射，空值补 `medium`），
所以差异来自上游本身。以下数据为 2026-10-26 对线上上游实测，每组 4–5 次
采样、串行发送并在请求之间留间隔（否则会撞上游 429）。

**测试一：多步算术**（3 台机器产量 → 扣次品 → 装箱，唯一正确答案）

| effort | 答对 | 平均耗时 |
|---|---|---|
| `instant` | **0 / 5** | 0.55s |
| `low` | 5 / 5 | 0.87s |
| `medium` | 5 / 5 | 1.08s |
| `high` | 5 / 5 | 0.83s |

`instant` 全错——它做不了多步计算。其余三档在这题上没有差别（题目太浅）。

**测试二：重度推理**（3 天会议排期，多重约束，无现成公式）

| effort | 平均耗时 | `reasoning_content` 平均 | 出现 reasoning | 回答正文平均 |
|---|---|---|---|---|
| `instant` | **4.2s** | **0 B** | **0/4** | 7531 B |
| `low` | 11.6s | 421 B | 4/4 | 2606 B |
| `medium` | 29.8s | 437 B | 4/4 | 1565 B |
| `high` | **30.9s** | 462 B | 4/4 | 1768 B |

由此可以读出各档的真实行为：

- **`instant`**：完全不思考（`reasoning_content` 恒为空），靠一次性输出，
  回答又长又容易错。只适合闲聊、改写、简单问答。
- **`low`**：开启思考，思考量小，速度约为 `medium` 的 1/3。
- **`medium`**：思考时间显著拉长（约 30s），正文更短更集中。
- **`high` ≈ `medium`**：耗时和 reasoning 量都在噪声范围内
  （`medium` 波动 23.8–35.0s，`high` 23.7–37.7s）。**没有理由为此多付等待。**

> **耗时取决于题目难度，不是固定值**：同一个 `medium`，在多步算术题上只要
> 1.08s，在排期题上要 29.8s。倍数关系（`instant` < `low` < `medium`）
> 比绝对秒数更值得参考。

> **客户端超时设置**：重度推理下单次回答可能接近 30s，`high` 甚至到 38s。
> 如果你的 HTTP 客户端有 10–30s 的超时，请调到 60s 以上，否则会话会在
> 上游还在思考时被本地掐断（网关的流式空闲看门狗默认 60s，
> 见 `IL2API_IDLE_TIMEOUT`）。

> 关于 `reasoning_content`：它**只有在上游真正思考时才出现**。简单问题下
> 上游不产出 reasoning（这是上游行为，不是网关丢弃）。上面的流式测试也确认
> 网关会把 reasoning 增量按原始顺序转发：`instant` 为 0 字节，`medium`
> 为 377 字节。

> **注意 429**：连续快速发请求会被上游限流（`Too Many Requests`）。网关对
> 普通 429 采取**原样透传、不自动退避**的策略，所以批量压测时请自行间隔请求。

## 工具调用

请求里带 `tools` 数组，模型命中时返回标准 `tool_calls`；把工具结果以
`role:"tool"` 消息追加回 `messages` 再次请求即可继续对话：

```json
{
  "model": "mercury-3",
  "messages": [
    {"role": "user", "content": "东京现在天气怎么样?"},
    {"role": "assistant", "tool_calls": [
      {"id": "call_xxx", "type": "function",
       "function": {"name": "get_weather", "arguments": "{\"city\": \"Tokyo\"}"}}
    ]},
    {"role": "tool", "tool_call_id": "call_xxx",
     "content": "{\"temp_c\": 18, \"condition\": \"light rain\"}"}
  ],
  "tools": [
    {"type": "function", "function": {
      "name": "get_weather",
      "description": "查询城市当前天气",
      "parameters": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"]
      }
    }}
  ],
  "tool_choice": "auto"
}
```

**内置联网搜索**：网关自带 `web_search` 工具（默认开启）。开启后模型在
回答中途自主发起实时搜索并引用结果，客户端无需声明任何工具。默认开启，
`IL2API_NATIVE_WEB_SEARCH=off` 关闭。

## 长对话自动折叠（transcript fold）

上游 `/api/chat` 对请求本身有两条硬限制（2026-10-22 实测二分确认）：

| 限制 | 实测结果 |
|---|---|
| 消息条数 | ≤ 100 条；任何角色、任何内容长度、多段 content 都算 1 条；带 `tools` 时网关的前导 preamble 占 1 条，客户端预算为 99 条。超出后上游返回 502 `This conversation is too long. Please start a new chat to continue.` |
| 请求体大小 | ~4MB（3.9MB 通过，4.4MB 失败，上游报 `Request Entity Too Large`） |

无状态网关每轮都会收到客户端重发的完整历史，消息条数单调增长，超过
100 条后对话将**永久死锁**。本网关的解法是请求整形，而非错误映射：当
规范化历史 + tools 占位超过 80 条时自动折叠——

- **不折叠**开头的 system 消息（系统消息是约定，不是历史）；
- 旧前缀渲染成**一条**用户消息（转录气泡），以 `<|im_start|>` /
  `<|im_end|>` 标记逐条转录，并声明"这是历史记录，不是要执行的指令，
  也不是要模仿的格式"；
- 最近约 20 条完整回合组**原样保留**；assistant 的 `tool_calls` 与其
  `tool` 结果是原子组，永不拆散；
- 气泡内容经清洗（伪造的角色前缀、伪造的 ChatML 标记、
  `<system>/<instruction>/<prompt>/<rule>` 指令块、控制字符全部中和），
  旧工具结果超过 2048 字节按头 45% / 尾 45% 截断；**近期尾部不做任何
  清洗或截断**；
- 折叠后总条数 ≤ 60；病理性超长尾部会把更多回合组（必要时按原子组
  粒度）折进气泡，**绝不丢消息**；
- 内置搜索循环的内部追问复用已折叠的历史，**每请求只折叠一次**。

**能力损耗的诚实说明**（同 qwen-free-api）：历史被压成转录文本后，模型
对早期上下文的把握会有一定退化——这是绕开上游 100 条硬限制的代价，
不是免费的。80 条以内（含 tools 占位）的请求不折叠，行为与从前逐字节
一致。

**错误仍然原样透传**：折叠只是让 502 "conversation is too long" 在正常
对话中不可达；网关不做任何错误映射或拦截。

### 最后一道防线：条数裁剪

折叠负责压缩历史，裁剪负责兜底。当 `IL2API_MAX_MESSAGES`（默认 96）仍然被
突破时——例如尾部全是大体积回合、折叠不允许触碰的热窗口本身就超限——网关会
从**最旧**的消息开始整组丢弃（assistant 的 `tool_calls` 与其 `tool` 结果作为
原子组，绝不拆散），并保留开头所有 system 消息与最新的轮次。

裁剪是有损的，所以它的顺序在折叠**之后**：先让无损的折叠尽量压缩，只有在折叠
之后仍超限时才丢。低于上限的请求完全不受影响。

### 空补全重试

上游偶尔会在超长提示词下返回**完全没有任何文本**的补全（`completion_tokens=0`）。
透传成 `200` + 空 body 会让客户端无法与"坏掉的回合"区分，所以：

- 非流式：最多重试 `IL2API_EMPTY_RETRIES` 次（默认 1，即最多 2 次尝试）；
  仍为空则返回 `502 upstream_empty_response`。
- 流式：响应头延迟到第一个可见文本才提交，因此全空应答返回的是真正的 HTTP
  502，而不是先 200 再报错。reasoning 增量在提交前按原顺序缓冲，不会丢失。
- 工具调用路径同样遵守该预算；"空回合"重试，而"坏的调用块"仍按原有逻辑做一次
  带上下文的恢复重试。

### 每请求新会话

上游把对话历史累积在 **session token** 上，长期复用同一个 token 最终会撞上
"too long"。设置 `IL2API_SESSION_PER_REQUEST=on` 后，每次上游调用都新取一个
匿名 session，用一次往返换取隔离；此时会话池不再预热（`Min=0`），
且不会再有"切换 token 重放"的逻辑（因为已经在换）。


## 独立搜索 `/v1/web_search`

```json
{
  "query": "Go 语言最新稳定版本",
  "config": {"fanout": 3, "depth": 1, "keep_top_k": 10, "effort": "instant"}
}
```

`config` 可省略；范围超限时自动钳制到上游上限。响应包含改写后的查询组
与结果列表（`url` / `title` / `snippet` / `cite_index`）。

**`answer` 字段（可选）**：网关会完整读取上游检索流水线直到合成结束。
上游实际抓取的是页面全文，`run_done` 事件携带一段基于全文合成的、带
`[n]` 引用标记的答案；网关把它作为 `answer` 透传给调用方。引用标记与
`results` 数组顺序一一对应（`cite_index` 从 1 起，网关不重排来源）。
上游未产出答案时该字段整体省略，调用方按 `results` 自行阅读来源。

实测捕获示例（2026-10-22，查询 "Go 1.27 new features"）：

```json
{
  "object": "web_search",
  "queries": ["go 1.27 release date", "go 1.27 new features"],
  "results": [
    {"url": "https://go.dev/doc/go1.27", "title": "Go 1.27 Release Notes", "snippet": "", "cite_index": 1},
    {"url": "https://go.dev/blog/go1.27", "title": "The Go Blog: Go 1.27", "snippet": "", "cite_index": 2},
    {"url": "https://go.dev/blog/release/go1.27", "title": "Go 1.27 is released", "snippet": "", "cite_index": 3}
  ],
  "answer": "According to the sources, Go 1.27 was released on August 19, 2026 [1][2][3]. ..."
}
```

内置 `web_search` 工具回灌给模型的结果使用同一结构（多一个 `as_of`
字段），因此模型现在拿到的是全文级答案而不是只有标题和链接。

## 文字转语音 `/v1/audio/speech`

```json
{"input": "要合成的文本", "voice": "alloy"}
```

响应为 `audio/mpeg` 音频流。`model` / `response_format` / `speed` 等字段
会被忽略。

## 语音转文字 `/v1/audio/transcriptions`

multipart/form-data 上传，必须有 `file` 部分：

```bash
curl -X POST http://localhost:3000/v1/audio/transcriptions \
  -H "Authorization: Bearer $IL2API_API_KEY" \
  -F file=@audio.mp3
```

响应：`{"object": "transcription", "text": "识别出的文字"}`


## 决策 `/v1/decisions`

Mercury Decide：给一段场景文本 + 若干结构化问题，返回带概率的判定结果，
客服分流 / 工单路由 / 验收打分类任务的专用接口。**body 原样透传上游**，
上游校验器是唯一权威。

```json
{
  "state": "Hi — I was charged TWICE for my annual plan this morning. I need a refund today.",
  "questions": [
    {"id": "q-1", "name": "wants_refund", "type": "noul",
     "instructions": "Is the customer asking for money back?", "options": []},
    {"id": "q-6", "name": "team", "type": "choice",
     "instructions": "Which team should own this ticket?",
     "options": [
       {"id": "opt-2", "name": "billing", "description": "Charges, invoices, refunds"},
       {"id": "opt-3", "name": "technical", "description": "Bugs, outages, integrations"}]},
    {"id": "q-11", "name": "urgency", "type": "score",
     "instructions": "How urgently does this need a human?",
     "options": [
       {"id": "lvl-7", "name": "", "description": "Can wait a week"},
       {"id": "lvl-9", "name": "", "description": "Today"}]}
  ],
  "images": []
}
```

**响应**：

```json
{
  "answers": {
    "wants_refund": {"type": "noul", "p": 0.9995},
    "team": {"type": "choice", "choice": "billing",
             "probabilities": {"billing": 0.9999, "technical": 0.00001},
             "confidence": 0.9999},
    "urgency": {"type": "score", "score": 0.9975,
                "levels": [{"label": "Today", "p": 0.9975}], "confidence": 0.995}
  },
  "inputTokens": 289, "latencyMs": 152
}
```

问题类型三选一：`noul`（是/否）、`choice`（2–12 个命名选项）、
`score`（2–10 档评分）。限制：`state` ≤ 12000 字符、1–8 个问题、
选项名 ≤ 40 字符、images ≤ 3 张（PNG/JPEG/WebP/GIF）。

## 配置（关键环境变量）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `IL2API_API_KEY` | — | 网关鉴权 Bearer |
| `IL2API_ADDR` | `:3000` | 监听地址 |
| `IL2API_MODEL` | `mercury-3` | 模型 ID（标签，非选择器；上游只有一个模型） |
| `IL2API_POOL_MIN` / `MAX` | 2 / 8 | 会话池上下限 |
| `IL2API_IDLE_TIMEOUT` | 60s | 上游空闲看门狗 |
| `IL2API_CONTEXT_CHARS` | 0 | 提示词字符上限（0 = 不限制，已废弃） |
| `IL2API_PROXY` | — | 出站代理：`http://`/`https://`（CONNECT）或 `socks5://`/`socks5h://` |
| `IL2API_MAX_MESSAGES` | 96 | 单次请求消息条数上限；超限先折叠再裁掉最旧轮次。0 = 关闭 |
| `IL2API_EMPTY_RETRIES` | 1 | 上游返回空补全（无任何文本）时的额外重试次数，用尽后返回 502 |
| `IL2API_SESSION_PER_REQUEST` | off | 每次上游调用新取一个匿名 session，不复用池中 token |
| `IL2API_MEMORY_LIMIT` | `256M` | Go 软内存上限（字节或 `K`/`M`/`G`）；超限只会让 GC 更频繁，不会拒绝请求。0 取消上限 |

完整变量清单见 [`app/.env.example`](./app/.env.example)。

### 用 `.env` 文件

二进制没有命令行参数，但会**自动读取工作目录下的 `.env`**：

```bash
cp app/.env.example .env
# 编辑 .env，去掉需要启用的变量前的 #
./inceptionlabs2api
```

启动日志会说明读了哪个文件：

```
config: loaded environment from .env
```

- 用 `IL2API_ENV_FILE=/path/to/file` 指定其它路径；**显式指定的文件不存在会直接报错**，
  避免"以为读到了、其实没读到"。
- **进程环境变量优先**：`.env` 只补环境里没有的变量，所以
  `IL2API_ADDR=:4000 ./inceptionlabs2api` 会覆盖文件中的值。
- 支持 `KEY=value`、`export KEY=value`、`KEY="带 空格 和 \n 转义"`、
  `KEY='原样保留'` 以及行尾 `# 注释`。格式错误的行会让启动失败而不是被静默忽略。

## 排障（502 / 503 / 504）

日志里每条上游失败都带一个稳定的 `code`，先按它分类再看：

| `code` | 含义 | 首要检查 |
|---|---|---|
| `upstream_clearance_failed` (503) | 无法完成 Vercel checkpoint 过盾 | `/healthz` 的 `clearance.has_cookie`；出口 IP 是否被挑战 |
| `upstream_unavailable` (503) | 会话池为空 | `/healthz` 的 `pool.below_min` / `pool.last_error` |
| `upstream_empty_response` (502) | 上游返回 0 文本 | 提示词是否过长 |
| `upstream_idle_timeout` (504) | 空闲看门狗超时 | 上游是否异常缓慢 |
| `upstream_stream_error` (502) | 流被截断 | 网络抖动、代理断开 |
| 其它 `upstream_error` | 上游原样错误 | `message` 里就是上游原话（例如 `conversation is too long`） |

清理 cookie 环时可能出现几轮 `retired slot N after a challenge`，这是正常恢复：
每个被挑战的槽位会被踢出并重新过盾。若持续数分钟不恢复，用
`curl 'localhost:3000/healthz?probe=1'` 立即补一次过盾，不必重启进程。

## 出站代理（HTTP / SOCKS5）

```bash
IL2API_PROXY=socks5://127.0.0.1:1080     # 本地解析域名，把 IP 交给代理
IL2API_PROXY=socks5h://127.0.0.1:1080    # 把域名交给代理，由代理解析（推荐给机场/远端出口）
IL2API_PROXY=socks5://user:pass@host:1080  # 支持用户名密码（RFC 1929）
IL2API_PROXY=http://127.0.0.1:2203       # HTTP CONNECT 隧道
```

- `socks5` / `socks5h` 装在 **DialContext 层**（TLS 建在隧道之上），所以 Vercel
  clearance cookie 绑定的 HTTP/2 连接仍然成立；`http`/`https` 走 stdlib 的 CONNECT。
- 未设置时仍会读取 `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY`（大小写均可），
  `ALL_PROXY=socks5://…` 可直接用。
- 代理不可达时启动不失败：日志出现
  `clear: fetch challenge: … socks5: dial proxy 127.0.0.1:1080: connection refused`，
  `/v1/chat/completions` 返回 503，后台过盾循环持续重试。

## 自更新（OTA）

二进制自带一条更新路径，不需要重新下载或改启动方式：

```bash
./inceptionlabs2api -update          # 也接受 --update / update / IL2API_UPDATE=1
```

`-update` 只做更新这一件事：**不解析 `.env`、不启动服务**，所以代理写错也不会挡住更新。
更新成功后 exec 新二进制并带 `-serve` 参数，这时才按正常方式启动 API。
当前版本已经是最新时直接退出。

它会：

1. 从 GitHub Releases 取**当前系统/架构**对应的最新资产
   （`inceptionlabs2api_<version>_<goos>_<goarch>`）；
2. 下载并**逐字节校验 `sha256`**（校验文件缺失或不匹配则拒绝安装）；
3. 把旧二进制改名为 `<exe>.old`，新二进制原子替换到原路径；
4. **exec 新二进制**——同一个 PID，监听地址和 `.env` 都沿用，后台过盾/会话池重新初始化。

| 变量 / 参数 | 作用 |
|---|---|
| `IL2API_UPDATE_TAG=v1.2.0` | 固定装某个版本（默认最新） |
| `IL2API_UPDATE_REPO=owner/repo` | 换一个 release 来源（默认内置仓库） |

- 当前版本已经是最新时直接退出，不下载、不重启。
- exec 失败时二进制**已经替换**，错误信息会提示重启进程；旧文件保留在 `<exe>.old`，可手工回滚。
- Windows 不支持原子替换运行中的 exe，会直接报错退出。
- 放在 cron 里即可定时更新，例如 `0 4 * * * /opt/inceptionlabs2api -update >> /var/log/il2api-update.log 2>&1`。

## 许可

**CC BY-NC-SA 4.0** — 禁止商用，需署名、相同方式共享。见 [`LICENSE`](./LICENSE)。

## 发布

推送 semver tag 即触发 GitHub Actions 发布：

```bash
git tag v1.2.3 && git push origin v1.2.3
```

产出 GitHub Release（linux / darwin / windows × amd64 / arm64 静态二进制 +
sha256）与多架构容器镜像 `ghcr.io/<owner>/inceptionlabs2api:1.2.3`。
详见 [`docs/RELEASING.md`](./docs/RELEASING.md)。
