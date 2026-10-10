# 本 fork 的目的与维护规则

上游:https://github.com/yetone/magpie(MIT)
基点:`d81706f9`(fork 时上游 HEAD,remote 名为 `upstream`)
分支:`xkeysmith`——所有自有改动落在此分支,`main` 保持与上游同步的干净基线。

## 目标

把 [xkeysmith](https://github.com/ryfineZ/codex-keysmith)(本地 LLM 语义网关:
L0–L4 递进改写、拒绝判定、审计重放、预算闸门)接入 magpie 的切换生态:

```
Codex / Claude Code / … ──► magpie(:3425,方言互转 + 切换 + 账号池)
                              │ 选中 xkeysmith 这个 provider 时
                              ▼
                          xkeysmith(:8787,语义管线 + 审计)
                              │
                              ▼
                     真上游(sub2api / 官方 API / 订阅)
```

## 补丁清单

| 补丁 | 内容 | 状态 |
| --- | --- | --- |
| 1 | `internal/provider/presets.go`:内置 `xkeysmith` preset(KindLocal,NoKey,Chat+Responses 双方言 base) | ✅ |
| 2(规划) | xkeysmith 侧:`/v1/models` 透传端点,让 magpie 自动列出真实上游模型 | 未做 |
| 3(规划) | sidecar 生命周期:复用 `internal/plugin/host.go` 的模式拉起/守护 xkeysmith 二进制 | 未做 |
| 4(规划) | 状态透出:xkeysmith 的 trace 头(`x-xkeysmith-trace/layer/escalated`)透传给客户端,GUI 显示语义层状态 | 未做 |

## 关键设计事实(改码前先读)

- magpie 的 provider 是**纯数据**(`internal/provider/provider.go`),无接口无工厂;
  "内置 provider" = `presets.go` 一条数据 + 网关按 vendor 的特判分支。
- relay 判定在 `internal/gateway/gateway.go:2696` 附近:入站方言 ∈ provider 可用方言
  且非订阅账号 ⇒ **原样透传**(不经 IR 翻译)。xkeysmith 同时声明 Chat+Responses base,
  因此两种 OpenAI 方言都以客户端原始形态到达 xkeysmith——审计保真的关键。
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
