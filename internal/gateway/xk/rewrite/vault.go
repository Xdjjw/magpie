package rewrite

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/yetone/magpie/internal/gateway/xk/govern"
)

// 占位符保险库:L4「目标抽象化」的落地实现。
//
// 三个必须做对的地方:
//  1. **不可碰撞**:占位符形如 «TGT-<nonce>-<n>»,nonce 每请求随机。
//     模型既猜不到也编不出,因此「还原」不会误伤模型自己生成的内容。
//  2. **去重**:同一个 IP 出现 10 次只占 1 个槽位,还原表不会膨胀。
//  3. **流式还原**:SSE 增量里占位符可能被切成两半,必须做滑动窗口保留尾部,
//     否则会漏还原(用户会看到 «TGT-… 残留)。

const (
	placeholderOpen  = "«"
	placeholderClose = "»"
)

// TargetKind 是目标值的种类(与 xp-core TargetKind 对齐)。
type TargetKind string

const (
	TargetURL    TargetKind = "url"
	TargetEmail  TargetKind = "email"
	TargetIPv4   TargetKind = "ipv4"
	TargetDomain TargetKind = "domain"
	TargetSerial TargetKind = "serial"
)

type TargetRule struct {
	Kind    TargetKind
	Pattern string
	Enabled bool
}

// DefaultTargetRules 与 xp-core default_target_rules 逐条对齐(顺序即优先级)。
func DefaultTargetRules() []TargetRule {
	return []TargetRule{
		{Kind: TargetURL, Pattern: `(?i)\bhttps?://[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]+`, Enabled: true},
		{Kind: TargetEmail, Pattern: `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`, Enabled: true},
		{Kind: TargetIPv4, Pattern: `\b(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\b`, Enabled: true},
		{Kind: TargetDomain, Pattern: `\b(?:[A-Za-z0-9-]+\.)+(?:com|net|org|io|dev|ai|co|cn|ru|top|xyz)\b`, Enabled: true},
		{Kind: TargetSerial, Pattern: `\b[A-Z0-9]{4}(?:-[A-Z0-9]{4}){2,7}\b`, Enabled: true},
	}
}

type compiledTarget struct {
	kind TargetKind
	re   *regexp.Regexp
}

type VaultEntry struct {
	Index       int        `json:"index"`
	Placeholder string     `json:"placeholder"`
	Original    string     `json:"original"`
	Kind        TargetKind `json:"kind"`
}

type MaskHit struct {
	Kind        TargetKind
	Placeholder string
	Original    string
}

type MaskOutcome struct {
	Text string
	Hits []MaskHit
}

func (o *MaskOutcome) Changed() bool { return len(o.Hits) > 0 }

type PlaceholderVault struct {
	prefix     string
	nonce      string
	entries    []VaultEntry
	byOriginal map[string]int
}

func NewPlaceholderVault(prefix string) *PlaceholderVault {
	nonce := strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	return &PlaceholderVault{prefix: prefix, nonce: nonce, byOriginal: map[string]int{}}
}

func (v *PlaceholderVault) Nonce() string         { return v.nonce }
func (v *PlaceholderVault) IsEmpty() bool         { return len(v.entries) == 0 }
func (v *PlaceholderVault) Len() int              { return len(v.entries) }
func (v *PlaceholderVault) Entries() []VaultEntry { return v.entries }

func (v *PlaceholderVault) placeholderText(index int) string {
	return fmt.Sprintf("%s%s-%s-%d%s", placeholderOpen, v.prefix, v.nonce, index, placeholderClose)
}

func (v *PlaceholderVault) intern(original string, kind TargetKind) string {
	if idx, ok := v.byOriginal[original]; ok {
		return v.entries[idx].Placeholder
	}
	index := len(v.entries)
	placeholder := v.placeholderText(index)
	v.entries = append(v.entries, VaultEntry{
		Index: index, Placeholder: placeholder, Original: original, Kind: kind,
	})
	v.byOriginal[original] = index
	return placeholder
}

// CompileTargets 编译目标规则。
func CompileTargets(rules []TargetRule) ([]compiledTarget, error) {
	var out []compiledTarget
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("target rule %s: %w", r.Kind, err)
		}
		out = append(out, compiledTarget{kind: r.Kind, re: re})
	}
	return out, nil
}

// Mask 把一个字符串里的目标值替换为占位符。
//
// 规则按配置顺序应用;后应用的规则不会重新命中已被替换掉的内容,
// 因此「URL 先于域名」这个顺序本身就是优先级声明。
func (v *PlaceholderVault) Mask(text string, rules []compiledTarget, protect []string) MaskOutcome {
	current := text
	var hits []MaskHit
	for _, rule := range rules {
		locs := rule.re.FindAllStringIndex(current, -1)
		if len(locs) == 0 {
			continue
		}
		// 从后往前替换:保持前面的字节索引仍然有效。
		for i := len(locs) - 1; i >= 0; i-- {
			start, end := locs[i][0], locs[i][1]
			original := current[start:end]
			if isProtected(original, protect) {
				continue
			}
			placeholder := v.intern(original, rule.kind)
			current = current[:start] + placeholder + current[end:]
			hits = append(hits, MaskHit{Kind: rule.kind, Placeholder: placeholder, Original: original})
		}
	}
	// hits 是倒序收集的,翻回来
	for i, j := 0, len(hits)-1; i < j; i, j = i+1, j-1 {
		hits[i], hits[j] = hits[j], hits[i]
	}
	return MaskOutcome{Text: current, Hits: hits}
}

// isProtected:本地地址、文档示例等不应被抽象化的字面量。
func isProtected(candidate string, protect []string) bool {
	for _, p := range protect {
		if p != "" && (candidate == p || strings.Contains(candidate, p)) {
			return true
		}
	}
	return false
}

// Restore 全文还原。
func (v *PlaceholderVault) Restore(text string) string {
	out := text
	for _, e := range v.entries {
		if strings.Contains(out, e.Placeholder) {
			out = strings.ReplaceAll(out, e.Placeholder, e.Original)
		}
	}
	return out
}

// RestoreValue 递归还原 JSON 文档里的**所有**字符串。
//
// 必须覆盖 tool_use.input / tool_call arguments 这类结构化字段——
// 模型可能把被抽象化的目标值放进工具参数里,漏还原会让工具调用拿到假地址。
func (v *PlaceholderVault) RestoreValue(value any) any {
	if len(v.entries) == 0 {
		return value
	}
	switch t := value.(type) {
	case string:
		return v.Restore(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = v.RestoreValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[v.Restore(k)] = v.RestoreValue(item)
		}
		return out
	default:
		return value
	}
}

// Restorer 生成一个增量还原器,供 SSE 转发使用。
func (v *PlaceholderVault) Restorer() *StreamRestorer {
	maxLen := 0
	pairs := make([][2]string, 0, len(v.entries))
	for _, e := range v.entries {
		if n := len([]rune(e.Placeholder)); n > maxLen {
			maxLen = n
		}
		pairs = append(pairs, [2]string{e.Placeholder, e.Original})
	}
	return &StreamRestorer{pairs: pairs, holdCap: maxLen + 16}
}

// StreamRestorer 跨 chunk 保留可能被切断的占位符尾部。
type StreamRestorer struct {
	pending string
	pairs   [][2]string
	holdCap int
}

func (s *StreamRestorer) HasMapping() bool { return len(s.pairs) > 0 }

// Push 投喂一个增量,返回**可以安全下发**的已还原文本。
func (s *StreamRestorer) Push(chunk string) string {
	if len(s.pairs) == 0 {
		return chunk
	}
	s.pending += chunk
	flushUpto := s.safeBoundary()
	if flushUpto == 0 {
		return ""
	}
	head := s.pending[:flushUpto]
	s.pending = s.pending[flushUpto:]
	return s.restore(head)
}

// Finish 流结束:把残留全部放出。
func (s *StreamRestorer) Finish() string {
	if s.pending == "" {
		return ""
	}
	tail := s.pending
	s.pending = ""
	return s.restore(tail)
}

func (s *StreamRestorer) restore(text string) string {
	out := text
	for _, p := range s.pairs {
		if strings.Contains(out, p[0]) {
			out = strings.ReplaceAll(out, p[0], p[1])
		}
	}
	return out
}

// safeBoundary 是安全切分点:绝不把「可能是半个占位符」的尾部放出去。
func (s *StreamRestorer) safeBoundary() int {
	open := strings.LastIndex(s.pending, placeholderOpen)
	if open < 0 {
		return len(s.pending)
	}
	tail := s.pending[open:]
	if strings.Contains(tail, placeholderClose) {
		// 已闭合,可以整段放出
		return len(s.pending)
	}
	if len([]rune(tail)) > s.holdCap {
		// 异常:迟迟不闭合,不能无限缓存
		return len(s.pending)
	}
	return open
}

// ─────────────────────────── L4 层 ───────────────────────────

// TargetAbstraction 是五层里**唯一一个「改回来」的层**,也是收益最干净的
// 一层:请求里只替换 URL/IP/域名/序列号,语义完全不变。
type TargetAbstraction struct {
	rules   []compiledTarget
	protect []string
}

func NewTargetAbstraction(rules []TargetRule, protect []string) (*TargetAbstraction, error) {
	compiled, err := CompileTargets(rules)
	if err != nil {
		return nil, err
	}
	return &TargetAbstraction{rules: compiled, protect: protect}, nil
}

func (l *TargetAbstraction) ID() govern.LayerID { return govern.L4 }

func (l *TargetAbstraction) Apply(env *Envelope, ctx *LayerCtx) (*LayerReport, error) {
	vault := ctx.Vault
	report := NewLayerReport(govern.L4)
	masked, slots := 0, 0
	var kinds []string
	env.forEachTextMut(ScopeAll, func(_ SlotCtx, text *string) {
		outcome := vault.Mask(*text, l.rules, l.protect)
		if outcome.Changed() {
			masked += len(outcome.Hits)
			slots++
			for _, hit := range outcome.Hits {
				kind := string(hit.Kind)
				if !containsStr(kinds, kind) {
					kinds = append(kinds, kind)
				}
			}
			*text = outcome.Text
		}
	})
	report.SlotsTouched = slots
	report.TargetsMasked = masked
	report.Changed = masked > 0
	for _, k := range kinds {
		report.Rule("l4.target:" + k)
	}
	if masked > 0 {
		report.Note(fmt.Sprintf("抽象化 %d 处目标(nonce=%s),响应阶段会还原", masked, vault.Nonce()))
	}
	return report, nil
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
