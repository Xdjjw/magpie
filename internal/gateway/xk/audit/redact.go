package audit

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

// 脱敏的目标不是「删掉」,而是「可关联」:命中的片段替换成
// ‹redacted:<短哈希>›,同一密钥在多次记录里保持同一个标记,
// 既能分析又不会泄漏。

// Redactor 持有一组编译好的脱敏正则。
type Redactor struct {
	rules []*regexp.Regexp
}

// NewRedactor 在构造期编译正则,写错立刻报错(启动期失败优于运行期失败)。
func NewRedactor(patterns []string) (*Redactor, error) {
	r := &Redactor{}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redact pattern %q: %w", p, err)
		}
		r.rules = append(r.rules, re)
	}
	return r, nil
}

func DisabledRedactor() *Redactor { return &Redactor{} }

func (r *Redactor) IsNoop() bool { return len(r.rules) == 0 }

func (r *Redactor) Redact(text string) string {
	if len(r.rules) == 0 {
		return text
	}
	out := text
	for _, re := range r.rules {
		if !re.MatchString(out) {
			continue
		}
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			return "‹redacted:" + ShortHash([]byte(m)) + "›"
		})
	}
	return out
}

// RedactValue 递归脱敏 JSON 值里的所有字符串。
func (r *Redactor) RedactValue(v any) any {
	if len(r.rules) == 0 {
		return v
	}
	switch t := v.(type) {
	case string:
		return r.Redact(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = r.RedactValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = r.RedactValue(item)
		}
		return out
	default:
		return v
	}
}

// RedactTruncated 按字节上限截断(脱敏之后再做,避免切断标记),
// 截断点回退到 UTF-8 边界。
func (r *Redactor) RedactTruncated(text string, maxBytes int) string {
	redacted := r.Redact(text)
	if len(redacted) <= maxBytes {
		return redacted
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(redacted[end]) {
		end--
	}
	return fmt.Sprintf("%s…[truncated %dB]", redacted[:end], len(redacted))
}

// DefaultRedactPatterns 与 xp-core policy.rs 的 default_redact_patterns 逐条对齐。
func DefaultRedactPatterns() []string {
	return []string{
		`(?i)\b(?:sk|rk|pk)-[A-Za-z0-9_\-]{12,}\b`,
		`(?i)\bBearer\s+[A-Za-z0-9._\-]{12,}`,
		"(?i)\\b(?:api[_-]?key|token|secret|password)\\b\\s*[=:]\\s*[\"']?[A-Za-z0-9._\\-]{8,}",
		`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`,
		`\bgh[pousr]_[A-Za-z0-9]{20,}\b`,
		`\bAKIA[0-9A-Z]{16}\b`,
	}
}
