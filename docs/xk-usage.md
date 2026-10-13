# xkeysmith 语义管线使用指南

magpie fork 在任意 LLM 上游之上**就地**叠加一层语义管线:L0–L4 改写阶梯、
拒绝判定、入口人格、审计。不是第二个代理进程——同一个 magpie,同一个网关
端口,你的 agent(Codex/Claude Code/…)照常接进来。

## 1. 安装

从 [Releases](https://github.com/Xdjjw/magpie/releases) 下载对应平台:

| 文件 | 平台 | 首次运行 |
|---|---|---|
| `magpie-macos-arm64.app.zip` | macOS 桌面(菜单栏) | 右键 → 打开(ad-hoc 签名) |
| `magpie-desktop-windows-*.exe` | Windows 桌面 | SmartScreen「仍要运行」 |
| `magpie-cli-*` | 全平台命令行 | 直接运行 |

## 2. 三步上手

**第一步:添加一个语义 provider。** `semantic=yes` 是关键——它标记这个
provider 的请求走语义管线。地址填**真实上游**(官方 API 或中转站),key
就是上游的 key:

```sh
magpie provider add review chat=https://api.example.com/v1 key=sk-xxx models=m1 semantic=yes
```

或者桌面端:设置 → Providers → 添加,勾选「启用语义流水线」。用
`magpie provider set <id> semantic=yes` 可以随时开关单个 provider。

**第二步:把 agent 指上来**(和普通 magpie 用法一致):

```sh
magpie codex review/m1      # Codex 走 review 的 m1,经语义管线
```

**第三步:一键开关。**

```sh
magpie xk on                # 管线开;人格为空时自动装推荐模板
magpie xk status            # on + persona 状态
```

托盘菜单和 设置 → 语义流水线 的开关是同一个动作。关(`xk off`)则所有
请求原样直连、人格还原,2 秒内生效,不用重启——管线出问题时一拨即断。

## 3. 客户端怎么接

**方式一:托管的 agent(推荐,Codex 属于这类)。** magpie 直接改写 agent
自己的配置,把它指到本地网关——agent 正常使用,完全无感:

```sh
magpie ls                    # 这台机器检测到的 agent
magpie codex review/m1       # Codex 的模型经 magpie(含语义管线)
magpie codex default         # Codex 还原为不经 magpie
```

桌面端的 Agents 页点选是同一件事。LAN 上其他机器的 agent 也能接:设置 →
General → Share on local network。

**方式二:任意 OpenAI 兼容客户端手动接。** 网关默认在
`http://127.0.0.1:3425`,四种协议方言都收(`/v1/chat/completions`、
`/v1/responses` 自动识别;Anthropic Messages、Gemini 同样支持):

```sh
magpie gateway-key add myclient    # 发一个客户端用的 key
```

客户端配置:Base URL `http://127.0.0.1:3425/v1`,API Key 用上面发的,
模型名写 `provider/model`(如 `review/m1`)。请求进来后和托管 agent 的
流量走同一条路:semantic=yes 的 provider 自动过 L0–L4。

网关的进程形态任选:桌面 app(开着就有)、`magpie serve`(无界面常驻)、
`magpie tui` / `magpie web`(终端/浏览器)。

## 4. 人格(persona):日常调参的主旋钮

人格是一段随每个语义请求从 L0 上行的 system 框架(你已授权的工作环境,
用你自己的话写),用来在第一次调用就避开关键词误杀:

```sh
magpie xk persona set - < docs/xk-persona.txt   # 上传文件(记住来源路径)
magpie xk persona show                          # 查看
magpie xk persona on|off                        # 开关:off 只停注入,文本保留
magpie xk persona clear                         # 清除(做对照基线)
```

设置页 → 会话人格 = 开关 + 上传文件(不再手写),保存即热生效。自己上传的
人格永远优先于模板;`xk on` 不会覆盖它。`xk off` 时,有可回读的源文件以
磁盘上的为准还原,没有则清空。写法要点见 `docs/xk-persona.md`。

## Codex 目录注入(客户端层,可选)

与会话人格独立的第二个注入位:把一个文件**整体替换** `~/.codex/AGENTS.md`
(Codex 的全局指令),原件自动备份为 `AGENTS.md.magpie-bak`:

```sh
magpie xk codex set - < 你的文件.md   # 替换(原件备份)
magpie xk codex on|off               # Off = 还原原件;On = 再写入
magpie xk codex clear                # 删除注入,原件回归
```

设置页「Codex 指令文件」行同样操作(开关 + 上传 + 清除)。它从 Codex 组装
请求时生效(客户端层),人格从网关转发时生效(网关层),**两层同时开就是
叠加**:两段文本都会在同一条请求里。建议两层放不同内容(Codex 层放交付
契约/路由规则,人格层放环境框架),避免重复花费 token。

## 5. 度量:拒绝率用数据说话

```sh
magpie xk stats          # 最近 500 条阶梯尝试
magpie xk stats 2000     # 更大窗口
```

输出四个关键信息:

- **request refusal rate**:至少一层被拒的请求占比——使用体感;
- **每层 refuse%**:L0 就被拒多 → 人格措辞或 L2 词面该调;L0 过 L1+ 拒
  → 升层在工作,正常;
- **decisions**:accepted/escalated/stopped 的比例;
- **not-passed families/signals**:驱动拒绝的检测族——`lexical` 词面族
  调人格/词面规则,`structured/transport` 是上游硬策略,提示词管不了。

调参闭环:`xk persona clear` 跑基线 → `stats` 记录 → 部署人格跑同样的活
→ 再 `stats` → 对比。

## 6. 看一次请求发生了什么

- **响应头**:`x-xkeysmith-trace`(追踪 id)、`x-xkeysmith-layer`(采纳
  到哪层,L0=直通即成功)、`x-xkeysmith-escalated`(是否升过层)、
  `x-xkeysmith-stop`/`-error`(终止原因)。
- **审计 JSONL**:每层尝试一条,含判定、命中规则、脱敏目标表、用量。
  位置:`~/.magpie/xkeysmith/audit/`(或对应平台的设置目录)。
- 每 `xk__mcp__*` 网关工具调用也在同一时间线上。

## 7. 管道内的秘密与工具(按需)

- 秘密/内网目标在 L2+ 自动换成 `«TGT…»` 占位符,回复回来 L4 还原——
  上游看不到原值,审计里有脱敏对照表。
- MCP 与技能(可选):`XKEYSMITH_TOOLS=1` 开工具平面,`XKEYSMITH_MCP_SERVERS`
  配 MCP(stdio/http),技能放 `~/.magpie/xkeysmith/skills/`,
  `XKEYSMITH_SKILLS_MODE=index|full`。模型经 `xk__mcp__<server>__<tool>`
  调用它们。

## 8. 状态文件一览

| 文件(设置目录下) | 作用 |
|---|---|
| `xkeysmith-master.json` | 总开关 |
| `xkeysmith-persona.json` | 人格文本 |
| `xkeysmith-power.json` | 一键开关的快照(还原用) |
| `xkeysmith/audit/*.jsonl` | 审计流水 |
| `xkeysmith/skills/` | 技能目录 |

设置目录:macOS `~/Library/Application Support/magpie`(或
`$XDG_CONFIG_HOME/magpie`),Linux `~/.config/magpie`。

## 9. 故障排查

| 症状 | 看哪 | 处置 |
|---|---|---|
| 请求大量 504「wall clock ran out」 | stats 的 stopped | 调大 provider 设置里的 WallClockMS |
| L0 拒绝率高 | families 榜 | 改人格措辞;词面命中的告诉我补 L2 规则 |
| 一律 4xx 且 families 是 transport | 上游返回体 | 硬策略拦截,换渠道或接受 |
| 管线行为异常 | `magpie xk off` | 急断直连,再排查 |

开发侧文档:`docs/xkeysmith-migration.md`(架构与迁移)、
`docs/xk-persona.md`(人格机制与模板来源)。
