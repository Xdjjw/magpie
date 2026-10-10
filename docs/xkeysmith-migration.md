# xkeysmith 语义平面移植设计(fork 内部文档)

前提校准:两个项目"功能类似"只对**传输层**成立(HTTP 服务、SSE、方言、provider 转发),
而 magpie 的传输层更强(四方言 + IR 互转)——**传输层留 magpie 的,xkeysmith 的
`xp-server`/`xp-provider`/`xp-cli` 不移植**。要移植的是 magpie 完全没有的部分:

| xkeysmith crate | 行数 | 去向(magpie 包) | 性质 |
| --- | --- | --- | --- |
| `xp-detect`(拒绝判定:信号融合→置信度三态) | 950 | `internal/gateway/xk/detect` | 纯函数,易移植 |
| `xp-govern`(预算/会话记忆/升级决策 plan) | 1056 | `internal/gateway/xk/govern` | 纯函数,易移植 |
| `xp-audit`(JSONL trace/脱敏/explain/replay) | 1343 | `internal/gateway/xk/audit` | 直接移植 |
| `xp-rewrite`(L0-L4 + 占位符保险库 + 流式还原) | 1737 | `internal/gateway/xk/rewrite` | L1-L3 易;L4 流式还原最难 |
| `xp-core`(policy 正则集/envelope/hash/ids) | ~1600 相关 | `internal/gateway/xk/policy` | 正则全部 RE2 兼容(已核验,无损) |
| skills/tools/MCP 平面 | 4251 | 阶段 4 再议 | 依赖 IR 工具形状,最大新工程 |

合计移植面 ≈ 6-7k 行 Rust → Go。**这是重写(port),不是搬运**;Rust 原版降级为
spec 与 test oracle(参照上游保留旧 WebSocket 网关做 oracle 的惯例)。

## 为什么不走 CGo/FFI

`make cli` 是 `CGO_ENABLED=0` 的 headless 交叉编译路线;引入 cdylib 依赖会毁掉
Windows/Linux 交叉编译与静态分发。WASM(wasi)+ Bun 插件宿主理论上可保留单一 Rust
真源,但流式状态跨边界 + 冷门调试路径,仅作保底研究,不选为主路线。

## 插入点(基于 fork 基点 d81706f9 的侦察)

- **改写(L1-L3)**:magpie IR 的 `Part{Kind, Text, Args json.RawMessage}`
  (`internal/gateway/ir.go`)天然等价于 `Envelope::for_each_text_mut` 的不变量——
  只遍历 `Kind==Text` 的 parts,`ToolCall.Args` 不是文本、不可触碰,且一套代码覆盖四方言。
  钩子:`translate` 路径在 `parse(from, body)`(`gateway.go:4411`)之后;
  `relay` 路径(原样透传,`gateway.go:2695` 判定)需要按方言的原始 JSON 访问器
  (xkeysmith envelope 的两种方言解析可直接移植),或在语义开关开启时对目标 provider
  强制 translate——**设计决策待定,倾向后者(简单、语义清晰,代价是失去原样字节)**。
- **升级循环**:xkeysmith 的「同 provider 内,判定拒绝→升层→重写→重试」发生在
  **单次 attempt 内部**(`gateway.go:2624` `s.attempt`),不与 magpie 的候选/故障转移
  循环(`plan`/`planGroup`)混淆——两层重试正交叠加。
- **判定**:挂在响应事件流(`internal/gateway/continuation.go:252` streamTranslated
  的 Event 序列);结构化证据从 HTTP 状态/错误体取,词面证据扫 text events。
- **审计**:每 attempt 一条记录;`capture.go`/`sniff.go` 已有逐流钩子可挂;
  落盘遵循 magpie 的 appdir 约定。
- **L4 流式还原**:跨 chunk 滑动窗口状态对象挂在 streamTranslated 的 per-stream
  上下文里,算法照移植,事件形状对齐 IR Event。

## 阶段计划(每阶段以语料为验收契约)

1. **detect + govern + audit**(纯函数三件套)——✅ 已完成(写侧):
   detect(82 条语料全绿,`409c4fe4`)、govern 33 测试(`ba51641d`)、
   audit 写侧(记录/脱敏/JSONL 落盘轮转/计数,reader 的 explain/replay
   随接入展示 UI 时移植)。合计 71 个 Go 测试与 Rust 侧一一对应。
2. **rewrite L1-L3**(文本槽上的确定性变换)——✅ 已完成:
   fence 代码感知切分(保护 fenced/行内代码与 URL)、L1 上下文伪装
   (指令上限/伪装轮次工具配对保护/Responses 的 instructions 分支)、
   L2 词面改写(只改用户轮次、跳代码)、L3 中立化重述(确定性映射表)、
   Pipeline 累积执行 + 深拷贝隔离。22 个测试,与 Rust 侧一一对应。
3. **升级循环接线 + L4 流式还原**(最难):attempt 内重试 + 占位符保险库 + 滑窗还原。
   验收:端到端(Codex→magpie→模拟拒绝上游)阶梯完整;replay 指纹与 Rust 版一致。
4. **skills/tools/MCP 平面**(最大新工程,届时再设计)。

## 双真源纪律("按需更新"的工程化)

- Go 版是 fork 的运行实现;Rust 仓库继续作为行为规范与 oracle 存在。
- 任何语义变更:先改 Rust + corpus,再同步 Go,oracle diff 必须清零。
- 上游同步时的高发冲突区:`gateway.go` 的 attempt/serve(我们加了钩子)、
  `ir.go`(若上游改 Part 形状,rewrite/detect 钩子要跟着动)。
