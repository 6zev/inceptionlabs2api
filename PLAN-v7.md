# PLAN v7: Cookie 环预热与轮换的流畅性优化（final)

## 目标（用户指令）
- 预热 20 个 cookie，429 即丢弃、后台重解 PoW 补位
- 不吃性能、不卡进程、体验流畅
- 不并发：异步慢慢来

## 调研结论（每条设计决策的出处）

1. **"Warm a base, not the max"** — asyncpg/pgbouncer 学派：boot 只暖一个基数，
   其余按需/后台补。放到我们这：**boot 只同步填第 1 个槽**（~100-400ms），
   服务立即可用；其余 19 个由唯一一个后台 goroutine 顺序补。
   （出处: async-concurrency.com "Warming Connection Pools at Startup" §Pitfalls
   — "warm a base of connections rather than the maximum"）
2. **Readiness 只等最小可用，不等满池** — 同文 §4：预热完成度不绑定就绪，
   部分可用即服务。我们：healthz 的 ok 在第 1 槽活时即 true，
   slots[] 展示每槽状态（filled/pending）供观察。
3. **单飞行后台补位（per-key single-flight refill）** — tg-ws-proxy-rs pool.rs：
   每次 pool.get() 后 spawn 一个 refill，但用 refilling HashSet 保证
   每个 key 同时只有一个 refill 在跑，防止 N 个并发 refill 打爆预算。
   我们：唯一的后台填充循环 = 全局单飞行，天然防踩踏；
   429 退役的槽只是把"目标活槽数 < Pool"重新变真，同一个循环会捡起来补。
4. **提交节奏 + 抖动（stagger/jitter）** — 爬虫池经验（proxyhat/apify）：
   每次提交间隔 1-2s + 随机抖动，20 个提交摊 30-60s，比业务流量温和得多。
   CPU（PoW 解）完全本地、一个一个解，不吃性能。
5. **maxLifetime 均匀摊开，不要突发刷新** — pgbouncer max_lifetime / Reactor
   maxLifeTime：连接到期后错峰关闭重开。我们：45min TTL 不变，但每槽的
   acquired 时间天然错开（boot 顺序填 + 补位时间不同），刷新循环维持
   一次一槽即可——20 槽 × 每 tick 一槽，tick 间隔 = 45min/20 ≈ 2min15s
   的节奏扫，全环在窗口内平滑刷完，零突发。

## 设计（SDD 改 SPEC §2.1.2）

- **槽结构**：`{cookie, acquiredAt, state: pending|live|cooldown}`，默认 20。
- **Boot**：main 里起服务前同步取第 1 个 cookie（失败不致命：服务照起，
  healthz ok=false，后台循环继续试——"a failed warm-up is not a failed startup"）。
- **后台填充循环**（唯一 goroutine）：
  `for { if liveCount < Pool { 尝试填下一个 pending 槽（含 PoW+提交）; sleep 1-2s+抖动 } else { sleep 5s } }`
  失败的槽进 cooldown，下轮再试，永不退出。
- **取用**：请求从 live 槽轮转（round-robin 指针，现有逻辑）。
- **429 退役**：请求路径上只做"标记退役 + 立刻换下一槽重放"（现有逻辑），
  补位交给后台循环——请求永不等待 PoW。
- **刷新**：槽年龄 > 45min 标记 stale，后台循环顺带换新（一次一槽）。
- **healthz**：`{live: n, pool: 20, slots: [...]}` 每槽 state+age。
- **零并发**：除后台循环外无新增 goroutine；PoW 逐个解；无锁竞争热点
  （槽数组一把 mutex，临界区只做指针交换）。

## TDD 测试点
1. boot：第 1 槽活即 healthz ok=true，其余 pending
2. 后台循环：mock 上游，验证顺序填充 + 间隔 + 抖动存在
3. 429：退役该槽 → 请求立即用下一槽 → 后台循环在 N 秒内补位到满
4. 刷新：槽龄超 45min 被后台换新，一次只换一个
5. 失败：填槽失败进 cooldown 重试，不影响其他槽与服务
6. 关停：shutdown 时后台循环干净退出（-race 无泄漏）

## 交付
SPEC §2.1.2 重写 commit → 实现 → 全绿 build/vet/test -race →
:3000 换新二进制，healthz 确认 20/20，push origin main。
README/.env/compose 同步默认值 20。
