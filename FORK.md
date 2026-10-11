# 本 fork 的目的与维护规则

上游:https://github.com/yetone/magpie(MIT)
基点:`d81706f9`(fork 时上游 HEAD,remote 名为 `upstream`)
分支:`xkeysmith`——所有自有改动落在此分支,`main` 保持与上游同步的干净基线。

## 目标

把 [xkeysmith](https://github.com/ryfineZ/codex-keysmith) 的语义能力
(L0–L4 递进改写、拒绝判定、审计重放、预算闸门)直接整合进 Magpie 的网关:

```
Codex / Claude Code / … ──► magpie(:3425,方言互转 + 切换 + 账号池)
                              │ provider.Semantic=true
                              ▼
                 L0→L4 语义管线 + 工具轮 + 审计(同一进程)
                              │
                              ▼
                     真上游(sub2api / 官方 API / 订阅)
```

## 补丁清单

| 补丁 | 内容 | 状态 |
| --- | --- | --- |
| 1 | `internal/provider/presets.go`:内置 `xkeysmith` preset(KindLocal,**endpoint 必填** = 用户自己的真上游;base_url 不再默认 8787) | ✅(v2) |
| 2 | ~~xkeysmith 侧 `/v1/models` 透传~~ —— 已撤销:迁移是**单向**的,magpie fork 不依赖也不改动 xkeysmith 仓库。模型清单用 `magpie provider add xkeysmith models=…` 手填。 | 撤销 |
| 3 | provider 级 `semantic` 开关:任意真上游都能启用内置 L0–L4 管线 | ✅ |
| 4 | 状态透出:`x-xkeysmith-trace` / `-layer` / `-escalated` / `-stop` 响应头,客户端据此关联审计记录 | ✅ |

## 语义平面(内置)

**semantic 开关(不绑定 provider 名)**:任何 provider 都能加本地语义管线——

```sh
magpie provider add myrelay url=https://真上游/v1 key=KEY models=… semantic=yes
magpie codex myrelay/<模型>      # 走 L0-L4 改写阶梯 + 判定 + 审计
magpie codex moonshot/kimi-k2.5  # 切走 = 直连,不过管线
```

- `semantic=yes` 后,该 provider 的请求在 magpie 进程内就地过语义管线;
  base URL 仍是**真上游**,没有第二层代理。
- GUI 编辑器同样有 semantic 开关;`xkeysmith` 预设(endpoint 必填)只是
  便捷入口,名字不再重要。

fork 内置了完整语义平面(见 `docs/xkeysmith-migration.md`)。满足
`provider.Semantic` 或使用 `xkeysmith` preset 的 provider 会走
`serveXkeysmith`:判定 → 升层(L0→L4) → 还原 → 审计；工具轮由网关自己执行。

## 关键设计事实(改码前先读)

- magpie 的 provider 是**纯数据**(`internal/provider/provider.go`),无接口无工厂;
  "内置 provider" = `presets.go` 一条数据 + 网关按 vendor 的特判分支。
- 语义入口在 `internal/gateway/gateway.go` 的 `attempt`:provider 开启
  `Semantic`(或是 `xkeysmith`) 后进入 `serveXkeysmith`;其内部复用
  `attemptNative`，保留 provider/model 映射、账号适配、协议转换和 usage。
- 每个客户端请求共享一个 budget、deadline、trace 和工具副作用状态；L0–L4
  的探测属于同一请求，工具已经执行后不会再由 Magpie 的候选循环重放。
- 本地地址(localhost/127.0.0.1)天然免 key(`provider.go` 的 `keyOptional`)。
- 上游演进方向是"订阅类 provider 移出核心进插件"(见 AGENTS.md);
  本 fork 的内置 xkeysmith 属私有增强,**不打算回传上游**。

## 与上游同步

```sh
git fetch upstream
git checkout main && git merge --ff-only upstream/main
git checkout xkeysmith && git rebase main   # 或 merge,视冲突口味
```

同步前先读上游 `AGENTS.md`/`LESSONS.md` 的更新;冲突高发区:presets.go、gateway.go。
构建验证:`make cli`(headless,CGO_ENABLED=0)与 `make test`。

## 上游要求(改动纪律)

改已文档化的子系统前读 `docs/subsystems/` 对应页;代码标准见 `docs/code-standards.md`;
提交信息遵循上游语义化描述惯例(参考 git log)。
