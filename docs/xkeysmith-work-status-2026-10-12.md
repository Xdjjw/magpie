# Magpie 与 xkeysmith 整合工作交接

更新时间：2026-10-12，Asia/Taipei。

当前架构采用 **Magpie 原生网关加同进程 xkeysmith 语义层**。核心接线已在整合分支提交，语义请求可以复用 Magpie 的模型映射、账号适配和协议转换。Skills 配置、请求超时和 MCP 后台初始化已收尾，完整套件（`go test -tags nogui ./...`）53 个包全绿；剩余的是真实上游联调、提交推送与产物重建（见下文「后续工作顺序」）。

本文记录该时点的代码、评审和验证状态，供继续开发使用。长期设计参考见 [迁移架构](xkeysmith-migration.md)、[网关路由](subsystems/gateway-routing.md) 和 [网关中间件](subsystems/gateway-middleware.md)。

## 工作区和提交

| 工作区 | 分支与 HEAD | 当前用途和状态 |
| --- | --- | --- |
| `/Users/jwjiang/PycharmProjects/codex-keysmith` | `main`，`cf50bb0` | Rust 原实现及行为参考；工作区干净 |
| `/Users/jwjiang/PycharmProjects/magpie` | `xkeysmith`，`e3e8c9c4` | 主工作树仍是旧接线；`xkeysmith-magpie-app.zip` 和 `xkeysmith-magpie-cli` 有本地修改 |
| `/Users/jwjiang/PycharmProjects/magpie-semantic-integration` | `codex/magpie-semantic-integration`，`aa00e2e4` | 后续代码工作的基础；本文写入前有 9 个未提交源码及测试文件 |

关键提交：

- `a31e7b23`：语义处理复用 Magpie 原生 provider transport，并补充协议、预算和工具边界测试。
- `aa00e2e4`：刷新当时的打包产物，位于上述接线提交之后。
- 本地记录的 `origin/codex/magpie-semantic-integration` 指向 `aa00e2e4`；本次整理未联网刷新远端引用。

主工作树的源码、整合分支的源码和已有二进制不能视为同一个版本。后续打包需要记录实际使用的源码提交；当前已有产物不能证明包含本轮未提交改动。

## 当前请求架构

```text
Agent 发出请求
  ↓
Magpie 中间件、路由、模型解析、provider 和账号选择
  ↓
attempt
  ├─ 普通 provider ───────────────────────────→ attemptNative
  └─ 总开关开启且 provider 启用 Semantic
       ↓
     serveXkeysmith
       ↓
     L0–L4 改写与判定循环
       ↓
     xkNativeProbe → attemptNative → 上游
       ↑                              ↓
       └── 缓冲响应、原生协议解码、拒绝判定
       ↓
     采纳响应 / 升层重试 / 停止
       ↓
     必要时执行网关工具轮
       ↓
     L4 还原、JSON 或 SSE 输出、审计记录
```

| 模块 | 责任 |
| --- | --- |
| Magpie | 路由、模型映射、provider 生命周期、账号适配、协议转换、原生传输和候选故障转移 |
| `xk/attach`、`xk/rewrite`、`xk/detect` | 改写阶梯、响应观察、判定、升级和还原 |
| `xk/govern` | 请求预算、升级决策、会话记忆和技能解锁状态 |
| `xk/skills`、`xk/tools` | 本地技能、工具声明、执行策略、网关工具轮和 MCP 桥接 |
| `xk/audit` | 每次探测及工具执行的审计记录，以及 explain/replay 等读侧能力 |

请求边界已经落在代码中：

- 同一个客户端请求共享 budget、deadline、trace 和工具副作用状态，覆盖候选切换、改写层及工具轮。
- 内层语义探测调用 `attemptNative`，避免再次进入 `attempt` 的语义分支。
- 已执行网关工具后，`xkActed` 阻止外层把请求重放到其他候选，避免重复副作用。
- 网关接管 Chat、Responses、Anthropic 的 `xk__` 工具。Gemini 保留原生工具路径，尚未接管网关工具。
- **所有语义探测都会先缓冲响应再判定**，因此语义 SSE 的首字节也要等待判定完成；该延迟不限于 L4。单次探测响应缓冲上限为 8 MiB。

实现入口见 [gateway.go](../internal/gateway/gateway.go) 的 `attempt` / `attemptNative`、[xkattach.go](../internal/gateway/xkattach.go) 的 `serveXkeysmith` 和 [xknative.go](../internal/gateway/xknative.go) 的 `xkRequest` / `xkNativeProbe`。

## 已提交的主要修复

整合分支已经包含以下工作：

- 语义请求恢复 Magpie 原生模型名映射、账号适配和跨协议转换。
- Chat、Responses、Anthropic、Gemini 的接线均有回归测试；支持对原生 SSE 进行观察、判定和输出。
- Responses、Anthropic 工具调用识别，以及混合工具轮的归属处理；网关专属工具由网关执行。
- 中文长文本会话键按 rune 截断，修复 token 重复计费。
- GUI 保存 provider 时保留并支持修改 `Semantic`；总开关使用原子缓存，修复已有并发读写问题。
- 请求共享预算和工具执行后的外层重试限制。

桌面端目前提供总开关和 provider Semantic 开关。Skills、Tools、MCP、预算及审计的完整桌面管理界面仍待实现。

## 本轮未提交修改

下表描述当前工作区实现；验证边界和剩余问题见后文。

| 修改 | 当前内容 | 相关文件 |
| --- | --- | --- |
| MCP JSON 配置 | 增加 `allow_tools` 等 snake_case 字段标签；JSON 缺省 `enabled` 为 true，显式 false 被保留 | `internal/gateway/xk/tools/plane.go`、`tools_test.go` |
| Skills 模式 | 增加 `XKEYSMITH_SKILLS_MODE`，区分 off、index、full；index 在工具平面不活跃时不注入；off 移除技能加载工具 | `internal/gateway/xkattach.go`、`xkcapability_test.go` |
| MCP 后台初始化 | `BuildAsync` 先建立本地工具，再后台连接 MCP；读取工具和连接状态时加锁并复制切片 | `internal/gateway/xk/tools/plane.go` |
| 请求工具快照 | 首次注入时冻结本请求可用工具集合；后台新发现的 MCP 工具供后续请求使用；会话解锁可以影响已冻结集合内的下一轮声明 | `internal/gateway/xk/tools/plane.go`、`internal/gateway/xkattach.go` |
| 请求超时 | `xkWriteStop` 区分墙钟超时、客户端取消及其他耗尽；阶梯转发返回后再次检查 context | `internal/gateway/xkattach_write.go`、`xkattach.go`、`xk/attach/ladder.go` |
| 回归测试 | 新增配置缺省值、异步初始化、技能模式、转发中超时及停止状态映射的测试 | 下列测试文件 |

本文写入前的 9 个修改文件为：

```text
internal/gateway/xk/attach/ladder.go
internal/gateway/xk/attach/ladder_test.go
internal/gateway/xk/tools/plane.go
internal/gateway/xk/tools/tools_test.go
internal/gateway/xkattach.go
internal/gateway/xkattach_write.go
internal/gateway/xkboundary_test.go
internal/gateway/xkcapability_test.go
internal/gateway/xkintegration_test.go
```

最后一处 `Plane.closed` 修改使 `Close` 幂等，并阻止后台成功连接在关闭后发布工具。**已复测**：
专项、race 与完整套件都通过（见下文「已执行验证」）。

## 配置现状

以下为整合工作区的当前实现，包含未提交修改。

| 配置 | 当前行为 |
| --- | --- |
| 语义总开关 | 默认开启；还需 provider 开启 `Semantic` 或匹配 xkeysmith 入口；CLI 和 GUI 可控制总开关 |
| `XKEYSMITH_SKILLS_DIR` | 缺省读取 `settings.Dir()/xkeysmith/skills` |
| `XKEYSMITH_SKILLS_MODE` | 缺省及未知值回落到 `index`；接受 `off`、`index`、`full`，off 另有 none/disabled 别名 |
| `index` | 有技能且工具平面活跃时，向 L1 注入索引；加载全文依赖技能工具，Gemini 的一致性待补齐 |
| `full` | 有技能时向 L1 注入受长度上限约束的技能内容，Tools 关闭也可使用；Tools 开启时仍保留技能加载器 |
| `XKEYSMITH_TOOLS` | 仅值为 `1` 时启用网关工具，缺省关闭 |
| `XKEYSMITH_APPROVAL` | 缺省 allow；值为 deny 时启用拒绝策略，由 allowlist 放行匹配工具 |
| `XKEYSMITH_ALLOWLIST` | 逗号分隔的工具名模式；在 allow 模式下不构成额外限制 |
| `XKEYSMITH_MCP_SERVERS` | MCP server JSON 数组；支持 stdio 和 HTTP，以及每个 server 的 `allow_tools` |
| 请求预算默认值 | 6 次上游调用、200000 token、120 秒墙钟时间 |
| 工具默认值 | 4 轮、单次调用超时 30 秒、结果长度上限 8000 字符 |

预算数字来自 `govern.DefaultLimits` 和 `tools.DefaultToolsConfig`。这些默认值不代表已经提供相应的桌面配置入口。Skills/Tools/MCP 配置在 `Server.xk()` 首次初始化时读取，目前没有完整的热重载流程。

## 剩余问题和验证目标

### 请求超时：已按真实形态修正（原判断需更正）

原文说「共享 deadline 到期也会进入 `r.Context().Err() != nil` 那条分支」——**不成立**。
墙钟 deadline 建在 `serveXkeysmith` 内部的派生 ctx 上（`xknative.go` 的 `xkRequest`），而
外层候选循环用的 `r.Context()` 只来自 `http.Server`，它只设了 `ReadHeaderTimeout` 与
`IdleTimeout`，没有 `WriteTimeout`、也没有 `TimeoutHandler`。所以共享 deadline 到期不会让
外层 `r.Context()` 变成 done，不会被误记成 499。

真正的缺口在邻近位置，已修：语义层写出的终止错误经 `holdWriter` 写出，而 `writeHeader`
对 `hold && code >= 400` 直接返回（`fallback.go`），于是 `hw.status` 记下了 503/504 而
`hw.ended` 仍为 false。外层随后按 `hw.failed()`（`retryable(5xx) == true`）把它当成「这个
候选失败了」，去问下一个候选 —— 语义管线在一个**共享预算已经花完**的请求上重跑一遍，
而写好的错误被扣在缓冲里，客户端拿到的是备选候选的答复。

修法：请求状态加 `terminal` 标记（`xknative.go` 的 `xkRequestState.terminal` / `xkTerminal`）；
`xkStop` 在 `reason.IsExhaustion()` 时置位（`xkattach_write.go`）；候选循环与 `xkActed`
并列地 `hw.release()`（`gateway.go`）。上游错误**不**置位——那正是 failover 存在的意义。

覆盖：`TestCapabilityExhaustionIsNotAskedOfAnotherCandidate`。无此修复时它失败于
`耗尽后仍在问另一个候选: spare=1 status=200 body={...content:"spare"...}` —— 客户端拿到的是
备选的 200，而不是语义层的 503。

仍未统一：504 响应的 `error.type` 是 `api_error`（`writeError` 没有 504 分支），
`xkeysmith_timeout` 只出现在 message 里。这是与 Rust 原版的契约差异。

### MCP 生命周期：已补齐

- `Build` 与 `BuildAsync` 曾各写一份过滤，同步那份漏了 `enabled`，于是「调用者挑了哪个
  入口」决定了一个被禁用的 server 会不会被连上。现在两者共用 `connectable`（`plane.go`）。
  覆盖：`TestDisabledServerIsNotConnected`。
- `Close` 之后，连接**成功**的分支会丢弃结果，**失败**的分支却照样往 `mcpStatus` 追加。
  现在两条路径都先看 `closed`。覆盖：`TestClosedPlanePublishesNothingFromALateConnection`。
- `XKEYSMITH_MCP_SERVERS` 解析失败曾静默返回空列表：一个写错的值表现成「一个 server 都没
  配」，而没有任何提示。现在 `xkLoadMcpServers` 返回错误，`xkLoadTools` 记一条 log。
  覆盖：`TestCapabilityMcpConfigErrorIsNotSilent`。
- MCP 连不上现在也会 log 一行（`connectMCP`）。此前只写进 `McpStatus`，而 `McpStatus()` 在
  生产代码里**没有任何读侧**（只有测试调它），所以「失败可见但不阻塞」是句空话。

### Skills 与工具可用性：一处默认行为需要确认

`index` 模式靠 `xk__skill_load` 取正文，工具平面没开时模型调不动它，所以不注入是对的
（`xkcapability_test.go` 的 `index requires loader` 用例说明这是有意为之）。但默认值组合
（`XKEYSMITH_SKILLS_MODE` 空 → index，`XKEYSMITH_TOOLS` 空 → 关闭）意味着**不设任何环境变量
时，技能目录里的内容一个字都不会进提示词**，而此前一声不吭。现在会 log 一行说明
（覆盖：`TestCapabilityIndexWithoutToolsSaysSkillsWereNotInjected`）。

**待拍板**：默认改成 `full`（自足、不依赖工具），还是保持 index 但把「技能未生效」暴露到
桌面端。当前只做到「不静默」。

Gemini 跳过网关工具注入但共用技能索引 pipeline 的不一致仍未处理。

### 既有红测试：已修（这一项此前被低估了）

完整套件在本次之前有 **4 个包 6 个用例失败**。与 `aa00e2e4` 逐包对比后可以分开：

| 用例 | 是否本轮引入 | 处理 |
| --- | --- | --- |
| `internal/agent` `TestFolderVarsAreListed` | **是**（新增的 `XKEYSMITH_SKILLS_MODE` 没登记进 `agentenv.Vars`） | 已修 |
| `internal/claudecode` `TestFindTakesTheMachinesOwnFirst` | 否 | 已修，见下 |
| `internal/gateway` `TestClaudeSubscriptionRunsTheDownloadedClaudeCode` | 否 | 已修，见下 |
| `internal/gateway` `TestClaudeSubscriptionWithoutClaudeCodeSaysHowToGetIt` | 否 | 已修，见下 |
| `internal/agent` `TestAsideOfflineDisconnectRestoresWithoutDaemon` | 否 | 已修，见下 |
| `internal/plugin` `TestPluginHTTP2TakesTheProxy` | 否 | 已修，见下 |

三处「测试摸到了真机」：

1. **Claude 三例**：`proc.FindTool` 在 `exec.LookPath` 失败后回落到 `UserBinDirs()`，而它
   无条件追加 `/opt/homebrew/bin` 与 `/usr/local/bin`。测试用 `t.Setenv("PATH", t.TempDir())`
   清空 PATH，挡不住机器上那份 `/opt/homebrew/bin/claude`。修法：把这两个前缀提成
   `proc.SystemBinDirs`（`find.go`，与 `appFolders` 同一手法），测试把它指到空。
2. **`TestAsideOfflineDisconnectRestoresWithoutDaemon`**：fixture 用 `ID: "dsh"`，而
   `HasApp()` 按 ID 查全局 `desktopApps` 并读 `/Applications`。这台机器装了
   `/Applications/DeepSeek Harness.app`，于是「没有 CLI」不成立。同一个测试目录里的姊妹用例
   `install_missing_test.go` 早就有 `appFolders = func() []string { return nil }` 这行，
   这一处是漏改的兄弟。
3. **`TestPluginHTTP2TakesTheProxy`**：与机器无关，是**夹具自身的 bug**。
   `testdata/fake/index.js` 的 `h2()` 在 `end` 里 `new Response(..., { status })`，而连接被
   对端关掉（不经过 `error`）时 `status` 仍是 0 → `RangeError` 抛在事件监听器里 →
   **整个 Bun 插件宿主退出（exit status 1）**。本机开着 127.0.0.1 的系统代理，失败形态正好是
   「干净地断开」而不是「报错」，于是必然踩中。修法：`status === 0` 时走既有的 `fail` 路径
   （502 + 原因），不再构造非法 Response。

### 文档与交付状态还需同步

- [迁移架构](xkeysmith-migration.md) 的「双真源纪律」已与 [FORK.md](../FORK.md) 统一：
  Rust 仓库是**行为参考**、不是构建依赖，语义变更在 Go 侧落地。
- 迁移文档里的 SSE 延迟说明仍只提了 L4，应覆盖所有语义探测。
- 主工作树仍停在旧接线（`e3e8c9c4`，只有两个二进制有本地修改）。整合时先辨认那两个产物
  修改，别把「换了二进制」当成源码合并。
## 已执行验证（本次收尾，隔离 HOME + 固定 Go 缓存）

命令模板见 [代码标准](code-standards.md) 第 2 条：临时 HOME，`GOPATH`/`GOMODCACHE`/`GOCACHE`
从 `go env` 取值。

| 检查 | 结果 |
| --- | --- |
| `gofmt -l`（改动文件） | 无输出 |
| `go vet ./...` | 通过 |
| `GOOS=linux go vet -tags nogui ./...` | 通过 |
| `GOOS=windows go vet ./...` | 通过 |
| `go build ./...` | 通过 |
| `GOOS=linux go build -tags nogui ./...` | 通过 |
| `GOOS=windows go build ./...` | 通过 |
| `go test -tags nogui ./...` | **53 个包全部通过，0 失败**（本次之前：4 个包 6 个用例失败） |
| `go test -race -tags nogui ./internal/gateway/xk/... ./internal/gateway -run 'Test(BuildAsync\|Mcp\|Xkeysmith\|Semantic\|Capability\|XkMaster\|LadderDeadlineDuringForward)' -count=1` | 通过，无 data race |
| 每个新用例「拿掉修复就会失败」 | 5 个新用例逐一验证过，失败信息见上文各节 |
| `make cli`（发布构建路径） | 通过；产物 `magpie` 39,124,962 字节，sha256 `03bf79e3b489c538db7849b92080843161dcf8cd1437469ea5be6180a6c15d35`，版本串 `v0.1.1157-47-gb4af0ed3` |
| 推送 | `origin/codex/magpie-semantic-integration`：`aa00e2e4..b4af0ed3`（fast-forward） |

**没有跑到的**：真实上游联调；GUI 的 Playwright 套件（本次没动 `internal/gui/assets`）；
Windows / Linux 实机（只做了交叉 vet 与 build，按 code-standards 那不算测试）。

## 分支关系（已核实）

`xkeysmith`（主工作树，`e3e8c9c4`）是 `codex/magpie-semantic-integration` 的**祖先**：
两者没有分叉，`xkeysmith` 没有任何整合分支缺少的提交，整合分支比它多 6 个提交
（`a31e7b23`、`aa00e2e4`，加上本次的 4 个）。主工作树那两个二进制的本地修改与源码无关。

## 后续工作顺序

1. 拍板 Skills 的默认模式（见上文「一处默认行为需要确认」）。
2. 对齐 504 的 `error.type` 契约，或明确记录「与 Rust 版不一致」是接受的。
3. Gemini 与技能索引 / full 模式的关系。
4. **从 `b4af0ed3` 重建 CLI 与桌面包**：`make cli` 已验证（校验和见上），
   但 fork 自己那两个产物（主工作树的 `xkeysmith-magpie-cli` 与
   `xkeysmith-magpie-app.zip`，停在 10-11 12:49）的打包配方没在仓库里，
   需要按原有流程重做，并记录提交号与校验和、核对安装后实际运行的版本。
5. 后续迭代补齐 Skills、Tools、MCP 和审计的桌面管理入口。

## 历史上下文

用户指定的 ZCode 对话已经定位：

- 会话：`zcodesess_24679883-f26c-4323-9e51-adebc0994270`。
- 原始记录：`/Users/jwjiang/.zcode/cli/rollout/model-io-sess_24679883-f26c-4323-9e51-adebc0994270.jsonl`。
- 早期评审稿：`/Users/jwjiang/.codex/visualizations/2026/10/11/01a1293b-28e7-7092-b5f2-33854871f6b6/magpie-architecture-review.md`。

早期评审针对较旧接线，其中原生 transport、模型映射和协议路径的主要问题已由 `a31e7b23` 处理；当前剩余事项以上文工作区状态和待办为准。
