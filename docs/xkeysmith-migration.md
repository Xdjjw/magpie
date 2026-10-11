# xkeysmith 语义平面移植与运行架构

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
| skills/tools/MCP 平面 | 4251 | `internal/gateway/xk/skills`, `xk/tools` | 工具轮、审批、MCP 与渐进披露 |

合计移植面 ≈ 6-7k 行 Rust → Go。**这是重写(port),不是搬运**;Rust 原版降级为
spec 与 test oracle(参照上游保留旧 WebSocket 网关做 oracle 的惯例)。

## 为什么不走 CGo/FFI

`make cli` 是 `CGO_ENABLED=0` 的 headless 交叉编译路线;引入 cdylib 依赖会毁掉
Windows/Linux 交叉编译与静态分发。WASM(wasi)+ Bun 插件宿主理论上可保留单一 Rust
真源,但流式状态跨边界 + 冷门调试路径,仅作保底研究,不选为主路线。

## 运行插入点

- **语义入口**:`internal/gateway/gateway.go` 的 `attempt` 检查
  `provider.Semantic` 或 `xkeysmith` preset，并转入 `serveXkeysmith`。
  该入口通过 `xkNativeProbe` 调用 Magpie 唯一的 `attemptNative`，因此保留
  provider/model 映射、账号适配、协议转换、usage 和 Gemini native stream 路径。
- **改写(L1-L3)**:`internal/gateway/xk/rewrite.Envelope` 按 wire 访问文本槽：
  Chat、Anthropic、Responses、Gemini 的 system、user、assistant、tool-result 文本
  可被规则改写；tool call 的 name、id、arguments 以及 Gemini functionCall 参数
  不在槽位集合内。L4 用 placeholder vault 保护目标，并在响应 JSON/SSE 上还原。
- **升级循环**:同一 provider 内的「判定拒绝→升层→重写→重试」由
  `internal/gateway/xk/attach.Run` 完成，不与 Magpie 的候选/故障转移循环混淆。
  每次探测先缓冲，只有被采纳的响应才释放；上游错误不会被误判为拒绝。
- **判定**:`xkObservation` 先用 Magpie 原生 decoder 将 whole JSON 或 SSE 统一
  成 IR，再读取 finish/stop reason、structured refusal、Gemini
  `promptFeedback.blockReason`、HTTP policy error 和文本证据交给 detector。
- **审计**:`internal/gateway/xk/audit` 每 attempt 一条 JSONL 记录，记录
  trace/session/wire/layer、判定、usage、请求/响应 hash 和脱敏 preview；启用
  `RawCapture` 时才保留原文。Gemini wire 也可由 replay 重新构造 envelope。
- **L4 流式还原**:语义探针本身会先缓冲完整响应再判定。采纳的流式响应在有
  L4 占位符时先解码成 Magpie IR、整体还原并按客户端方言重新编码，避免占位符
  被切在 SSE 事件之间；没有占位符时直接释放原生事件。这样保持事件形状正确，
  代价是 L4 流请求的首字节要等判定完成。

## 阶段计划(每阶段以语料为验收契约)

1. **detect + govern + audit**(纯函数三件套)——✅ 已完成(含读侧):
   detect(82 条语料全绿,`409c4fe4`)、govern 33 测试(`ba51641d`)、
   audit 写侧(记录/脱敏/JSONL 落盘轮转/计数)+ **读侧**
   (explain 阶梯表 / stats / tail / **replay 配置漂移检测**)。
2. **rewrite L1-L3**(文本槽上的确定性变换)——✅ 已完成:
   fence 代码感知切分(保护 fenced/行内代码与 URL)、L1 上下文伪装
   (指令上限/伪装轮次工具配对保护/Responses 的 instructions 分支)、
   L2 词面改写(只改用户轮次、跳代码)、L3 中立化重述(确定性映射表)、
   Pipeline 累积执行 + 深拷贝隔离。22 个测试,与 Rust 侧一一对应。
3. **升级循环接线 + L4 流式还原**——✅ 已完成:
   L4 目标抽象化 + 占位符保险库(不可碰撞 nonce / 去重 / 保护清单)+
   跨事件安全还原;attach 包实现阶梯运行器(判定→升层→重试,
   预算封顶,上游故障不升级)与响应侧还原(非流式整体 / 流式增量);
   网关接线:provider 的 `semantic` 开关或 xkeysmith preset 走语义通路
   (`serveXkeysmith`)，每条 attempt 一条审计记录；Chat、Responses、Anthropic、
   Gemini 均有真实 gateway 回归。attach 9 测试 + rewrite 29 测试。
4. **skills/tools/MCP 平面**——✅ 已完成:
   skills 包(SKILL.md 解析,含块状列表/注释/行内注释边界)+
   tools 包(xk__ 命名空间、审批闸门 + glob、渐进式披露门控、
   混合轮延迟客户端工具并先执行网关工具、技能加载执行器、MCP stdio/Streamable HTTP 桥含 cwd 预检与
   allow_tools 白名单);技能注入 L1、工具 schema 按解锁状态注入请求体(Chat、Responses、
   Anthropic)。Gemini 当前保留原生工具轮，不接管网关工具。19 测试。

## 双真源纪律("按需更新"的工程化)

- Go 版是 fork 的运行实现;Rust 仓库继续作为行为规范与 oracle 存在。
- 任何语义变更:先改 Rust + corpus,再同步 Go,oracle diff 必须清零。
- 上游同步时的高发冲突区:`gateway.go` 的 attempt/serve(我们加了钩子)、
  `ir.go`(若上游改 Part 形状,rewrite/detect 钩子要跟着动)。
