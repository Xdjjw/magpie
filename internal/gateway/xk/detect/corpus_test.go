package detect

import (
	"fmt"
	"sort"
	"testing"
)

// 语料回归:与 xp-detect/tests/corpus.rs 逐条对齐,是这次移植的**行为契约**。
//
// 判定器是整个语义平面的单点故障:漏判会让功能失效,误判会把成本放大 5 倍。
// 单元测试只能覆盖我们想到的形状,所以这里放一份标注语料:
//
//   - refusals:必须判为拒绝(含中文省略主语的续接式拒绝、英文第一人称拒绝);
//   - traps:必须不升级——它们包含「像拒绝」的词,但其实是正常技术输出。
//
// traps 是这份语料真正的价值所在,大多来自实际踩过的坑。硬性约束:
// traps 的置信度必须**低于升级阈值**,而不是仅仅「小于 1.0」——
// 只差一点点的误判在真实流量里等于误判。
//
// 同步纪律:语料变更先落 Rust 版(corpus.rs),再同步到这里,两边必须同绿。

type ccase struct {
	id   string
	text string
}

var refusals = []ccase{
	// ── 中文:显式第一人称 ─────────────────────────────────────────
	{"cn.cannot-help", "抱歉，我无法完成这个请求"},
	{"cn.violates-policy", "对不起，这违反了我们的政策"},
	{"cn.cannot-help-you", "我不能帮助您"},
	{"cn.cannot-do-this", "我不能做这个。"},
	{"cn.cannot-do-it", "我无法做这件事。"},
	{"cn.wont-help", "我不会帮你做这个。"},
	{"cn.cannot-provide", "我无法提供这个信息。"},
	{"cn.violates-my", "违反了我的安全准则"},
	{"cn.refuse", "我拒绝执行这个操作。"},
	{"cn.must-refuse", "我必须拒绝这个请求。"},
	{"cn.as-ai", "作为一个人工智能，我无法协助这个请求。"},
	{"cn.sorry-cannot-assist", "很抱歉，我不能协助你完成这个任务。"},
	// ── 中文:省略主语的续接式拒绝(最容易漏的一类) ───────────────
	{"cn.cannot-continue", "不能继续做这件事。上一轮请求不能继续处理。"},
	{"cn.cannot-continue-handle", "无法继续处理这件事，所以到此为止。"},
	// ── 英文:第一人称 ─────────────────────────────────────────────
	{"en.sorry-cannot", "Sorry, I cannot help with that"},
	{"en.apologize-violates", "I apologize, but this violates policy"},
	{"en.as-an-ai-unable", "As an AI, I'm unable to assist"},
	{"en.must-decline", "I must decline this request."},
	{"en.must-refuse", "I must refuse to do that."},
	{"en.unable-to-assist", "I'm unable to assist with that."},
	{"en.wont-help", "I won't help with that."},
	{"en.against-guidelines", "That is against my guidelines."},
	{"en.sorry-but-cant", "I'm sorry, but I can't do that."},
	{"en.as-language-model", "As a language model, I cannot provide that."},
	{"en.not-permitted", "I am not permitted to help with that."},
	{"en.you-not-allowed", "You are not allowed to do that."},
	{"en.violates-usage-policy", "This request violates my usage policy."},
	{"en.not-comfortable", "I'm not comfortable helping with that."},
	{"en.cannot-assist-request", "I cannot assist with this request."},
}

var traps = []ccase{
	// 同一批词出现在肯定句里
	{"trap.continue-old-api", "这个循环不能继续使用旧接口，我改成新接口。"},
	{"trap.keep-going", "好的，我来继续做这件事。"},
	{"trap.forbidden-config", "禁止在生产环境使用明文密钥。"},
	{"trap.disallowed-origin", "不允许的请求来源"},
	{"trap.policy-engine", "The policy engine rejects malformed requests."},
	{"trap.violates-spec", "违反了协议规范时，客户端会收到 400。"},
	{"trap.ai-researcher", "As an AI safety researcher, I would design the eval like this."},
	{"trap.meta-mention", "The phrase \"as an AI\" appears in many refusal templates."},
	{"trap.performance", "我无法满足这个性能要求，改用异步方案。"},
	{"trap.sorry-confusion", "Sorry for the confusion — the flag is `--verbose`."},
	{"trap.parser-unable", "The parser is unable to parse trailing commas."},
	{"trap.schema-not-permitted", "This is not permitted by the schema."},
	{"trap.token-not-allowed", "The API returns 403 when the token is not allowed to access the resource."},
	{"trap.refuse-to-accept", "I refuse to accept that the benchmark is meaningful."},
	{"trap.refused-pr", "我拒绝了这个 PR，因为缺少测试。"},
	// 带反证(已开始交付)的回复
	{"trap.cannot-promise-but-delivers", "I cannot promise it compiles, but here it is:\n```rust\nfn f() {}\n```"},
	{"trap.here-is-plan", "Here's the plan.\n```python\nprint('hi')\n```\nStep 1: ship it."},
	{"trap.short-answer", "42"},
	{"trap.sure-can-help", "Sure, I can help with that"},
	{"trap.good-question", "这是一个很好的问题"},
	{"trap.spanish-like", "No puedo creer lo simple que es esto."},
}

func TestEveryRefusalIsDetected(t *testing.T) {
	d := DefaultOrPanic()
	var missed []string
	for _, c := range refusals {
		v := d.JudgeText(200, c.text)
		if v.Outcome != Refused {
			missed = append(missed, fmt.Sprintf("%s: outcome=%s conf=%.2f text=%s", c.id, v.Outcome, v.Confidence, c.text))
		}
	}
	if len(missed) > 0 {
		t.Fatalf("漏判 %d / %d 条:\n%s", len(missed), len(refusals), join(missed, "\n"))
	}
}

func TestNoTrapIsEscalated(t *testing.T) {
	d := DefaultOrPanic()
	cfg := d.Config()
	var falsePositives []string
	for _, c := range traps {
		v := d.JudgeText(200, c.text)
		// 关键:不是「小于 1.0」,而是**低于升级阈值**。
		if v.Confidence >= cfg.EscalateThreshold {
			falsePositives = append(falsePositives,
				fmt.Sprintf("%s: outcome=%s conf=%.2f (阈值 %.2f) text=%s", c.id, v.Outcome, v.Confidence, cfg.EscalateThreshold, c.text))
		}
	}
	if len(falsePositives) > 0 {
		t.Fatalf("误判 %d / %d 条:\n%s", len(falsePositives), len(traps), join(falsePositives, "\n"))
	}
}

func TestEveryRefusalCarriesAtLeastOneNamedSignal(t *testing.T) {
	// 判定必须**可解释**:只给一个布尔值的判定器无法被审计。
	d := DefaultOrPanic()
	for _, c := range refusals {
		v := d.JudgeText(200, c.text)
		if len(v.Signals) == 0 {
			t.Fatalf("%s 判为拒绝却没有留下任何证据", c.id)
		}
		for _, s := range v.Signals {
			if s.Tag() == "" {
				t.Fatalf("%s 的证据缺少稳定 id", c.id)
			}
		}
	}
}

// 第三方拒绝短语表的覆盖面基线:取自 codex-session-patcher(MIT)的
// STRONG_REFUSAL_PHRASES——一个已在真实会话里用过的短语集合,
// 覆盖面不再由我们的直觉决定。
func TestThirdPartyPhraseBaselineIsCovered(t *testing.T) {
	baseline := []string{
		"我无法协助这个请求",
		"我无法帮助您",
		"我不能协助你",
		"我不能帮助您",
		"我不能做这个",
		"我无法做这件事",
		"我不会帮你做这个",
		"我无法提供这个信息",
		"不能继续做这件事",
		"无法继续做这件事",
		"不会继续做这件事",
		"不能继续处理这件事",
		"无法继续处理这件事",
		"违反了我的政策",
		"我拒绝执行",
		"我必须拒绝这个请求",
		"I cannot assist with this.",
		"I can't assist with that.",
		"I'm unable to assist with that.",
		"I cannot help with that.",
		"I can't help with that.",
		"I'm unable to help with that.",
		"I must decline.",
		"I must refuse.",
		"This is against my guidelines.",
		"This violates my policy.",
		"I won't help with that.",
		"I won't assist with that.",
		"I apologize, but I can't do that.",
		"I'm sorry, but I can't do that.",
		"not able to provide that",
		"not able to assist with that",
	}
	d := DefaultOrPanic()
	var missed []string
	for _, text := range baseline {
		v := d.JudgeText(200, text)
		if v.Outcome != Refused {
			missed = append(missed, fmt.Sprintf("%s -> %s (%.2f)", text, v.Outcome, v.Confidence))
		}
	}
	if len(missed) > 0 {
		t.Fatalf("第三方基线漏判 %d / %d 条:\n%s", len(missed), len(baseline), join(missed, "\n"))
	}
}

func TestCorpusIsNotTriviallySmall(t *testing.T) {
	// 防止有人「修 bug」时把语料删到只剩两条。
	if len(refusals) < 25 {
		t.Fatalf("拒绝语料太少:%d", len(refusals))
	}
	if len(traps) < 20 {
		t.Fatalf("误判陷阱语料太少:%d", len(traps))
	}
}

func TestTrapMarginIsReported(t *testing.T) {
	// 打印每条 trap 的实际置信度,便于调整规则时看到「离阈值还有多远」。
	d := DefaultOrPanic()
	cfg := d.Config()
	type margin struct {
		conf float64
		id   string
	}
	worst := make([]margin, len(traps))
	for i, c := range traps {
		worst[i] = margin{d.JudgeText(200, c.text).Confidence, c.id}
	}
	sort.Slice(worst, func(a, b int) bool { return worst[a].conf > worst[b].conf })
	t.Logf("trap 置信度最高的 5 条(阈值 %.2f):", cfg.EscalateThreshold)
	for i := 0; i < 5 && i < len(worst); i++ {
		t.Logf("  %.2f  %s", worst[i].conf, worst[i].id)
	}
	if worst[0].conf >= cfg.EscalateThreshold {
		t.Fatalf("最高 trap 置信度 %.2f 已过阈值", worst[0].conf)
	}
}

func join(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
