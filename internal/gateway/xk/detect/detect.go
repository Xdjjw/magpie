// Package detect is the refusal judge, ported from xkeysmith's xp-detect
// (crates/xp-detect, Rust) as part of this fork's semantic plane. The Rust
// crate stays the spec and the test oracle; any semantic change lands there
// first, then here, with the corpus (corpus_test.go) as the contract.
//
// 「被拒」不是词面现象,而是多层证据的合力:
//
//	结构化   refusal 字段 / finish_reason=content_filter / stop_reason=refusal   0.95
//	传输层   HTTP 400/403 + policy 错误码                                        0.90
//	词面模板 前缀窗口内的拒绝开场句                                            0.62~0.75
//	行为     空正文 / 仅 reasoning / 极短且无反证                            0.175~0.60
//
// 融合:score = max(权重) + 0.08×(不同族数-1),再被反证抑制。
// 反证(如已出现代码块)能压掉词面证据,但压不动结构化证据——
// 那是刻意留的地板:宁可多花一次调用,也不能漏判导致假成功。
package detect

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Observation is the raw evidence a verdict is judged on: plain data,
// no HTTP anywhere, so the judge stays testable without a network.
type Observation struct {
	Status        int
	FinishReason  string // OpenAI choices[].finish_reason / Anthropic stop_reason
	RefusalField  string // OpenAI message.refusal
	ErrorCode     string
	ErrorMessage  string
	Text          string
	ToolCalls     int
	ReasoningOnly bool
}

// Signal families. The number of distinct families feeds the corroboration
// bonus; the order below is the canonical order of Verdict.Families.
type Family string

const (
	FamStructured Family = "structured"
	FamTransport  Family = "transport"
	FamPrefix     Family = "prefix"
	FamBehavioral Family = "behavioral"
	FamAccept     Family = "accept"
)

var familyOrder = []Family{FamStructured, FamTransport, FamPrefix, FamBehavioral, FamAccept}

// Signal is one piece of evidence. Only one of Source/Code/RuleID/Detail is
// set, per Family; Tag is the stable label audit and explain tables use.
type Signal struct {
	Family  Family
	Source  string // structured
	Code    string // transport
	RuleID  string // prefix / accept
	Detail  string // behavioral
	Weight  float64
	Excerpt string
}

func (s Signal) Tag() string {
	switch s.Family {
	case FamStructured:
		return "structured:" + s.Source
	case FamTransport:
		return "transport:" + s.Code
	case FamPrefix:
		return "prefix:" + s.RuleID
	case FamBehavioral:
		return "behavioral:" + s.Detail
	default:
		return "accept:" + s.RuleID
	}
}

// Outcome is a deliberate three-state result: the escalation policy gets to
// handle the uncertain band explicitly instead of a boolean hiding it.
type Outcome string

const (
	Passed    Outcome = "passed"
	Uncertain Outcome = "uncertain"
	Refused   Outcome = "refused"
)

// Verdict is what the judge returns for one upstream observation.
type Verdict struct {
	Outcome    Outcome
	Confidence float64
	// Escalate is whether the next layer should be tried (uncertain_escalates
	// already accounted for).
	Escalate bool
	// UpstreamOK is whether the upstream call itself succeeded (HTTP < 400);
	// the engine uses it to tell a failed layer from a failed upstream.
	UpstreamOK bool
	Status     int
	Families   []Family
	Signals    []Signal
	Reason     string
}

// Acceptable: the attempt can be adopted — passed AND the upstream is fine.
func (v *Verdict) Acceptable() bool { return v.Outcome == Passed && v.UpstreamOK }

func (v *Verdict) HasStructuredEvidence() bool {
	for _, f := range v.Families {
		if f == FamStructured || f == FamTransport {
			return true
		}
	}
	return false
}

func (v *Verdict) RuleIDs() []string {
	out := make([]string, len(v.Signals))
	for i, s := range v.Signals {
		out[i] = s.Tag()
	}
	return out
}

func (v *Verdict) summarize() {
	if len(v.Signals) == 0 {
		v.Reason = fmt.Sprintf("%s (no signal)", v.Outcome)
		return
	}
	tags := make([]string, 0, 3)
	for _, s := range v.Signals {
		if len(tags) == 3 {
			break
		}
		tags = append(tags, s.Tag())
	}
	v.Reason = fmt.Sprintf("%s conf=%.2f via %s", v.Outcome, v.Confidence, strings.Join(tags, ", "))
}

// ─────────────────────────── 配置(与 xp-core DetectCfg 对齐) ───────────────────────────

type PatternScope string

const (
	ScopePrefix   PatternScope = "prefix"   // only the first PrefixWindowChars runes
	ScopeAnywhere PatternScope = "anywhere" // whole text
)

type Pattern struct {
	ID      string
	Regex   string
	Scope   PatternScope
	Weight  float64
	Enabled bool
}

type Weights struct {
	Structured float64
	Transport  float64
	Prefix     float64
	Behavioral float64
}

type Config struct {
	EscalateThreshold    float64 // ≥ → refused
	AcceptThreshold      float64 // < → passed; in between is uncertain
	UncertainEscalates   bool
	PrefixWindowChars    int
	BehavioralMinChars   int
	Weights              Weights
	Patterns             []Pattern
	AcceptPatterns       []Pattern
	TransportPolicyCodes []string
}

func DefaultConfig() Config {
	return Config{
		EscalateThreshold:  0.55,
		AcceptThreshold:    0.30,
		UncertainEscalates: true,
		PrefixWindowChars:  400,
		BehavioralMinChars: 24,
		Weights: Weights{
			Structured: 0.95,
			Transport:  0.90,
			Prefix:     0.70,
			Behavioral: 0.35,
		},
		Patterns:       defaultRefusalPatterns(),
		AcceptPatterns: defaultAcceptPatterns(),
		TransportPolicyCodes: []string{
			"content_policy_violation",
			"content_filter",
			"content_policy",
			"policy_violation",
			"safety",
		},
	}
}

func pat(id, re string, weight float64) Pattern {
	return Pattern{ID: id, Regex: re, Scope: ScopePrefix, Weight: weight, Enabled: true}
}

// defaultRefusalPatterns 与 xp-core policy.rs 的 default_refusal_patterns 逐条对齐。
// 改这里之前先改 Rust 版并跑语料——这里只做同步。
func defaultRefusalPatterns() []Pattern {
	return []Pattern{
		// ── 英文:第一人称拒绝 ──────────────────────────────────────
		pat("r.cannot-assist", `(?i)\bI (?:can(?:no|')t|won't|will not|am unable to)\b`, 0.72),
		pat("r.sorry-cannot", `(?i)\bI(?:'m| am) sorry,? but I\b`, 0.68),
		pat("r.apologize-but", `(?i)\bi apologize,? but\b`, 0.65),
		pat("r.must-decline", `(?i)\bI must (?:decline|refuse)\b`, 0.72),
		pat("r.not-able-to-help", `(?i)\b(?:unable|not able) to (?:help|assist|provide|comply)\b`, 0.70),
		pat("r.not-permitted", `(?i)\b(?:i(?:'m| am)|you(?:'re| are)) not (?:permitted|allowed) to\b`, 0.68),
		pat("r.as-an-ai", `(?i)\bas (?:an AI|a language model|an assistant)\b[^.\n]{0,40}\b(?:can(?:no|')t|won't|unable|not able)`, 0.62),
		pat("r.against-guidelines", `(?i)\b(?:violates?|against)\b[^.\n]{0,30}\b(?:my|our)\b[^.\n]{0,20}\b(?:policy|policies|guidelines|terms)\b`, 0.66),
		pat("r.not-comfortable", `(?i)\bnot comfortable (?:helping|assisting|with)\b`, 0.66),
		// ── 中文:带人称的显式拒绝 ──────────────────────────────────
		pat("r.cn-wufa", `我(?:无法|不能|没办法)(?:协助|帮助|提供|满足(?:你的|您的要求|这个请求|该请求))`, 0.75),
		pat("r.cn-baoqian", `抱歉[，,]?\s*我(?:无法|不能)`, 0.75),
		pat("r.cn-buneng-zuo", `我(?:不能|无法|不会)做(?:这个|这件事|这些|这种事)`, 0.72),
		pat("r.cn-buhui-bang", `我不会(?:帮|协助|支持|配合)`, 0.72),
		pat("r.cn-jujue", `我(?:必须|只能)?拒绝(?:执行|这个请求|该请求|你的请求|这个要求|继续|协助|帮助|提供)`, 0.72),
		pat("r.cn-weifan", `(?:违反|有悖)[^。\n]{0,10}(?:我的|我们的|安全政策|内容政策|使用政策|服务政策|平台政策|社区准则|服务条款)`, 0.70),
		pat("r.cn-zuowei-ai", `作为(?:一个)?(?:AI|人工智能|语言模型)[^。\n]{0,20}(?:无法|不能)`, 0.62),
		// ── 中文:省略主语的续接式拒绝(最易漏的一类) ───────────────
		pat("r.cn-jixu", `(?:不能|无法|不会)继续(?:做|处理|执行)这`, 0.72),
	}
}

func defaultAcceptPatterns() []Pattern {
	return []Pattern{
		{ID: "a.code-fence", Regex: "```", Scope: ScopeAnywhere, Weight: 0.85, Enabled: true},
		pat("a.here-is", `(?i)^\s*(?:here(?:'s| is)|sure[,!]|certainly|below is)`, 0.55),
		pat("a.cn-haode", `^\s*(?:好的|可以|没问题|以下是)`, 0.55),
	}
}

// ─────────────────────────── 规则编译 ───────────────────────────

type CompiledRule struct {
	ID     string
	Re     *regexp.Regexp
	Scope  PatternScope
	Weight float64
}

type Rules struct {
	Refusal []CompiledRule
	Accept  []CompiledRule
	// TransportCodes are lowercased policy error codes.
	TransportCodes []string
}

// Compile fails on a bad regex at startup rather than mid-request.
func Compile(cfg *Config) (*Rules, error) {
	r := &Rules{TransportCodes: make([]string, len(cfg.TransportPolicyCodes))}
	for i, c := range cfg.TransportPolicyCodes {
		r.TransportCodes[i] = strings.ToLower(c)
	}
	var err error
	if r.Refusal, err = compileSet(cfg.Patterns); err != nil {
		return nil, err
	}
	if r.Accept, err = compileSet(cfg.AcceptPatterns); err != nil {
		return nil, err
	}
	return r, nil
}

func compileSet(patterns []Pattern) ([]CompiledRule, error) {
	var out []CompiledRule
	for _, p := range patterns {
		if !p.Enabled {
			continue
		}
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("pattern %s: %w", p.ID, err)
		}
		w := p.Weight
		if w < 0 {
			w = 0
		} else if w > 1 {
			w = 1
		}
		scope := p.Scope
		if scope == "" {
			scope = ScopePrefix
		}
		out = append(out, CompiledRule{ID: p.ID, Re: re, Scope: scope, Weight: w})
	}
	return out, nil
}

// Detector holds the compiled rules; the hot path allocates only excerpts.
type Detector struct {
	cfg   Config
	rules *Rules
}

func New(cfg *Config) (*Detector, error) {
	rules, err := Compile(cfg)
	if err != nil {
		return nil, err
	}
	return &Detector{cfg: *cfg, rules: rules}, nil
}

func NewDefault() (*Detector, error) {
	cfg := DefaultConfig()
	return New(&cfg)
}

// DefaultOrPanic is for tests and fixed setups: the default config must
// always compile.
func DefaultOrPanic() *Detector {
	d, err := NewDefault()
	if err != nil {
		panic("default detect config must compile: " + err.Error())
	}
	return d
}

func (d *Detector) Config() *Config { return &d.cfg }

// Judge maps one upstream observation to a verdict.
func (d *Detector) Judge(obs *Observation) Verdict {
	cfg, rules := &d.cfg, d.rules
	var signals []Signal

	// 1. 结构化:上游自己承认了。
	if r := strings.TrimSpace(obs.RefusalField); r != "" {
		signals = append(signals, Signal{Family: FamStructured, Source: "refusal_field",
			Weight: cfg.Weights.Structured, Excerpt: excerpt(r)})
	}
	if fr := strings.ToLower(strings.TrimSpace(obs.FinishReason)); fr != "" {
		switch fr {
		case "content_filter", "safety", "blocked", "refusal", "prohibited_content":
			signals = append(signals, Signal{Family: FamStructured, Source: "finish_reason",
				Weight: cfg.Weights.Structured, Excerpt: excerpt(obs.FinishReason)})
		}
	}

	// 2. 传输层策略错误。
	if obs.Status >= 400 {
		hay := strings.ToLower(obs.ErrorCode + " " + obs.ErrorMessage)
		for _, c := range rules.TransportCodes {
			if c != "" && strings.Contains(hay, c) {
				signals = append(signals, Signal{Family: FamTransport, Code: c, Weight: cfg.Weights.Transport})
				break
			}
		}
	}

	// 3. 词面模板(只看前缀窗口,避免正文引用拒绝句而误判)。
	window := takeRunes(obs.Text, cfg.PrefixWindowChars)
	for _, rule := range rules.Refusal {
		target := window
		if rule.Scope == ScopeAnywhere {
			target = obs.Text
		}
		if m := rule.Re.FindString(target); m != "" {
			signals = append(signals, Signal{Family: FamPrefix, RuleID: rule.ID,
				Weight: rule.Weight, Excerpt: excerpt(m)})
		}
	}

	// 4. 反证:模型已经在交付东西了。
	acceptMax := 0.0
	for _, rule := range rules.Accept {
		target := window
		if rule.Scope == ScopeAnywhere {
			target = obs.Text
		}
		if rule.Re.MatchString(target) {
			acceptMax = max(acceptMax, rule.Weight)
			signals = append(signals, Signal{Family: FamAccept, RuleID: rule.ID, Weight: rule.Weight})
		}
	}

	// 5. 行为信号。
	trimmed := strings.TrimSpace(obs.Text)
	bodyChars := len([]rune(trimmed))
	if trimmed == "" && obs.ToolCalls == 0 && obs.Status < 400 {
		signals = append(signals, Signal{Family: FamBehavioral, Detail: "empty_completion",
			Weight: min(cfg.Weights.Behavioral+0.25, 1.0)})
	} else if trimmed != "" && bodyChars < cfg.BehavioralMinChars && obs.ToolCalls == 0 && acceptMax == 0 {
		// 极短但非空:单独不足以升级(权重刻意压在阈值下),只让已有词面证据更容易过线。
		signals = append(signals, Signal{Family: FamBehavioral, Detail: "very_short_completion",
			Weight: cfg.Weights.Behavioral * 0.5})
	}
	if obs.ReasoningOnly && obs.ToolCalls == 0 {
		signals = append(signals, Signal{Family: FamBehavioral, Detail: "reasoning_only",
			Weight: min(cfg.Weights.Behavioral+0.15, 1.0)})
	}

	// ── 融合打分 ──────────────────────────────────────────────────
	base := 0.0
	structured := false
	inFamily := map[Family]bool{}
	for _, s := range signals {
		if s.Family == FamAccept {
			continue
		}
		base = max(base, s.Weight)
		inFamily[s.Family] = true
		if s.Family == FamStructured || s.Family == FamTransport {
			structured = true
		}
	}
	families := make([]Family, 0, len(inFamily))
	for _, f := range familyOrder {
		if inFamily[f] {
			families = append(families, f)
		}
	}
	score := 0.0
	if len(families) > 0 {
		score = min(base+0.08*float64(len(families)-1), 1.0)
	}
	if acceptMax > 0 {
		suppression := acceptMax * 0.9
		if structured {
			suppression = acceptMax * 0.2
		}
		score *= 1 - suppression
	}
	// 地板:结构化证据不允许被反证压到阈值以下。
	if structured {
		score = max(score, base*0.85)
	}

	var outcome Outcome
	switch {
	case score >= cfg.EscalateThreshold:
		outcome = Refused
	case score < cfg.AcceptThreshold:
		outcome = Passed
	default:
		outcome = Uncertain
	}
	escalate := cfg.UncertainEscalates
	switch outcome {
	case Refused:
		escalate = true
	case Passed:
		escalate = false
	}

	v := Verdict{
		Outcome:    outcome,
		Confidence: math.Round(score*1000) / 1000,
		Escalate:   escalate,
		UpstreamOK: obs.Status < 400,
		Status:     obs.Status,
		Families:   families,
		Signals:    signals,
	}
	v.summarize()
	return v
}

// JudgeText is the convenience entry: status and text only.
func (d *Detector) JudgeText(status int, text string) Verdict {
	return d.Judge(&Observation{Status: status, Text: text})
}

// ─────────────────────────── 文本工具 ───────────────────────────

// takeRunes takes the first n runes — characters, not bytes — so a window
// boundary never splits a UTF-8 sequence.
func takeRunes(s string, n int) string {
	if len(s) <= n {
		return s // fewer bytes than the window ⇒ fewer runes, no split possible
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// excerpt flattens to one line and caps at 96 runes, for audit tables.
func excerpt(s string) string {
	oneLine := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	trimmed := strings.TrimSpace(oneLine)
	if len([]rune(trimmed)) <= 96 {
		return trimmed
	}
	return string([]rune(trimmed)[:96]) + "…"
}
