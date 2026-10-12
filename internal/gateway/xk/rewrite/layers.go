package rewrite

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yetone/magpie/internal/gateway/xk/govern"
)

// LayerReport 是单层改写报告。审计与 explain 表直接消费它。
type LayerReport struct {
	Layer         govern.LayerID `json:"layer"`
	Changed       bool           `json:"changed"`
	SlotsTouched  int            `json:"slots_touched"`
	TargetsMasked int            `json:"targets_masked"`
	// Rules 是命中的规则 id(不是正则本身——审计要引用稳定的 id)。
	Rules []string `json:"rules"`
	// Notes 是这一层**自己说它做了什么 / 为什么没做**,价值在静默失败可见。
	Notes      []string `json:"notes"`
	BytesDelta int      `json:"bytes_delta"`
}

func NewLayerReport(layer govern.LayerID) *LayerReport {
	return &LayerReport{Layer: layer, Rules: []string{}, Notes: []string{}}
}

func (r *LayerReport) Rule(id string) {
	for _, x := range r.Rules {
		if x == id {
			return
		}
	}
	r.Rules = append(r.Rules, id)
}

func (r *LayerReport) Note(text string) { r.Notes = append(r.Notes, text) }

func (r *LayerReport) Summary() string {
	if !r.Changed {
		return r.Layer.Label() + " no-op"
	}
	return fmt.Sprintf("%s slots=%d masked=%d rules=%d",
		r.Layer.Label(), r.SlotsTouched, r.TargetsMasked, len(r.Rules))
}

// LayerCtx 是层执行上下文:唯一可变的共享物是占位符保险库。
type LayerCtx struct {
	Vault   *PlaceholderVault
	Attempt uint8
}

// Layer 是一层改写。apply 就地修改 envelope,并回报它做了什么。
type Layer interface {
	ID() govern.LayerID
	Apply(env *Envelope, ctx *LayerCtx) (*LayerReport, error)
}

// ─────────────────────────── L0 直通 ───────────────────────────

type Passthrough struct{}

func (Passthrough) ID() govern.LayerID { return govern.L0 }
func (Passthrough) Apply(_ *Envelope, _ *LayerCtx) (*LayerReport, error) {
	return NewLayerReport(govern.L0), nil
}

// ─────────────────────────── L1 上下文伪装 ───────────────────────────
// skillsIndexMode is the skills mode whose payload is an index of skills the
// model turns into full text by calling the skill loader. The gateway leaves
// Gemini's tools alone, so that index names a capability a Gemini request
// cannot reach.
const skillsIndexMode = "index"

type ContextInjection struct {
	Instructions     string
	Primer           []PrimerTurn
	InjectSystem     bool
	MaxInjectedChars int
	// SkillsPayload 由调用方(阶段④的技能平面)渲染好传入;空 = 无技能。
	SkillsPayload string
	SkillNames    []string
	SkillsMode    string
}

func (l *ContextInjection) ID() govern.LayerID { return govern.L1 }

func (l *ContextInjection) Apply(env *Envelope, _ *LayerCtx) (*LayerReport, error) {
	report := NewLayerReport(govern.L1)

	if l.InjectSystem && strings.TrimSpace(l.Instructions) != "" {
		text, truncated := l.boundedInstructions()
		if env.AppendSystemText(text) {
			report.Changed = true
			report.SlotsTouched++
			report.Rule("l1.lab-instructions")
			if truncated {
				report.Note(fmt.Sprintf("lab instructions 截断到 %d 字符", l.MaxInjectedChars))
			}
		} else {
			report.Note("system 槽位不可写,跳过指令注入")
		}
	}

	if l.SkillsPayload != "" {
		if env.Wire == WireGemini && l.SkillsMode == skillsIndexMode {
			report.Note("Gemini 不接管网关工具,index 模式的技能索引不注入")
		} else if env.AppendSystemText(l.SkillsPayload) {
			report.Changed = true
			report.SlotsTouched++
			report.Rule("l1.skills-" + l.SkillsMode)
			for _, name := range l.SkillNames {
				// 逐个技能记进审计:事后要能回答「这次注入了哪几个技能」。
				report.Rule("skill:" + name)
			}
			report.Note(fmt.Sprintf("注入 %d 个技能(%s 模式)", len(l.SkillNames), l.SkillsMode))
		} else {
			report.Note("system 槽位不可写,跳过技能注入")
		}
	}

	if len(l.Primer) > 0 {
		n, err := env.PrependPrimerTurns(l.Primer)
		switch {
		case err != nil:
			report.Note("伪装轮次被拒绝: " + err.Error())
		case n > 0:
			report.Changed = true
			report.SlotsTouched += n
			report.Rule("l1.primer-turns")
			report.Note(fmt.Sprintf("注入 %d 条伪装轮次", n))
		default:
			report.Note("伪装轮次与原文首条同角色,已合并")
		}
	}
	return report, nil
}

func (l *ContextInjection) boundedInstructions() (string, bool) {
	max := l.MaxInjectedChars
	if max < 64 {
		max = 64
	}
	r := []rune(l.Instructions)
	if len(r) <= max {
		return l.Instructions, false
	}
	return string(r[:max]), true
}

// ─────────────────────────── L2 词面改写 ───────────────────────────

type RuleScope int

const (
	// OutsideCode 跳过 fenced code block、行内代码与 URL(默认,保护代码语义)。
	OutsideCode RuleScope = iota
	// Anywhere 全文改写。
	Anywhere
)

type TamperRule struct {
	ID            string
	Pattern       string
	Replacement   string
	Scope         RuleScope
	Enabled       bool
	UserTurnsOnly bool
}

type compiledTamper struct {
	id            string
	re            *regexp.Regexp
	replacement   string
	scope         RuleScope
	userTurnsOnly bool
}

type LexicalTamper struct{ rules []compiledTamper }

func NewLexicalTamper(rules []TamperRule) (*LexicalTamper, error) {
	l := &LexicalTamper{}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("tamper rule %s: %w", r.ID, err)
		}
		l.rules = append(l.rules, compiledTamper{
			id: r.ID, re: re, replacement: r.Replacement,
			scope: r.Scope, userTurnsOnly: r.UserTurnsOnly,
		})
	}
	return l, nil
}

func (l *LexicalTamper) ID() govern.LayerID { return govern.L2 }

func (l *LexicalTamper) Apply(env *Envelope, _ *LayerCtx) (*LayerReport, error) {
	report := NewLayerReport(govern.L2)
	l.applyGroup(env, ScopeUserTurns, true, report)
	l.applyGroup(env, ScopeAll, false, report)
	report.Changed = report.SlotsTouched > 0
	return report, nil
}

func (l *LexicalTamper) applyGroup(env *Envelope, scope Scope, userTurnsOnly bool, report *LayerReport) {
	var group []compiledTamper
	for _, r := range l.rules {
		if r.userTurnsOnly == userTurnsOnly {
			group = append(group, r)
		}
	}
	if len(group) == 0 {
		return
	}
	fired := map[string]bool{}
	slots := 0
	env.forEachTextMut(scope, func(_ SlotCtx, text *string) {
		touched := false
		for i := range group {
			rule := &group[i]
			var rewritten string
			if rule.scope == OutsideCode {
				rewritten = mapOutsideCodeAndURLs(*text, func(seg string) string {
					return rule.re.ReplaceAllString(seg, rule.replacement)
				})
			} else {
				rewritten = rule.re.ReplaceAllString(*text, rule.replacement)
			}
			if rewritten != *text {
				fired[rule.id] = true
				*text = rewritten
				touched = true
			}
		}
		if touched {
			slots++
		}
	})
	report.SlotsTouched += slots
	for id := range fired {
		report.Rule(id)
	}
}

// ─────────────────────────── L3 中立化重述 ───────────────────────────

type MapRule struct{ From, To string }

type NeutralBridge struct {
	Preamble string
	Map      []MapRule
	compiled []struct {
		re   *regexp.Regexp
		from string
		to   string
	}
}

func NewNeutralBridge(preamble string, rules []MapRule) (*NeutralBridge, error) {
	b := &NeutralBridge{Preamble: preamble, Map: rules}
	for _, r := range rules {
		if r.From == "" {
			continue
		}
		pattern := "(?i)" + regexp.QuoteMeta(r.From)
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("bridge map %q: %w", r.From, err)
		}
		b.compiled = append(b.compiled, struct {
			re   *regexp.Regexp
			from string
			to   string
		}{re, r.From, r.To})
	}
	return b, nil
}

func (b *NeutralBridge) ID() govern.LayerID { return govern.L3 }

func (b *NeutralBridge) neutralize(text string) string {
	return mapOutsideCode(text, func(seg string) string {
		current := seg
		for _, m := range b.compiled {
			current = m.re.ReplaceAllString(current, m.to)
		}
		return current
	})
}

func (b *NeutralBridge) Apply(env *Envelope, _ *LayerCtx) (*LayerReport, error) {
	report := NewLayerReport(govern.L3)

	// 前言只加一次,加在**最后一条用户消息**上:那才是本次真正的指令。
	lastUserPointer := ""
	env.forEachTextRef(ScopeUserTurns, func(ctx SlotCtx, s string) {
		if strings.TrimSpace(s) != "" {
			lastUserPointer = ctx.Pointer
		}
	})

	fired := map[string]bool{}
	slots := 0
	env.forEachTextMut(ScopeUserAndSystem, func(ctx SlotCtx, text *string) {
		touched := false
		for _, m := range b.compiled {
			if m.re.MatchString(*text) {
				fired["l3.map:"+m.from] = true
			}
		}
		if neutralized := b.neutralize(*text); neutralized != *text {
			*text = neutralized
			touched = true
		}
		if ctx.Pointer == lastUserPointer && b.Preamble != "" {
			*text = b.Preamble + *text
			touched = true
		}
		if touched {
			slots++
		}
	})

	report.SlotsTouched = slots
	report.Changed = slots > 0
	for id := range fired {
		report.Rule(id)
	}
	if report.Changed {
		report.Rule("l3.neutral-preamble")
		report.Note("用户/system 文本已中立化重述(代码块内不改写)")
	}
	return report, nil
}
