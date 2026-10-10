// Package skills is the capability plane's A-tier, ported from xkeysmith's
// xp-skills (Rust): a registry over agentskills.io-style SKILL.md files,
// rendered into a system-prompt payload at L1.
//
// 注入发生在 L1,所以 L0 仍是 pristine(审计重放的基线不被污染)。
// 默认关闭:技能正文是第三方文本,注入 system 等于把不受控内容放进
// 最高优先级位置。
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mode 是注入模式。
type Mode string

const (
	ModeOff   Mode = "off"
	ModeIndex Mode = "index"
	ModeFull  Mode = "full"
)

// Severity 是诊断级别。
type Severity string

const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
)

// DiagnosticKind 与 Rust 侧的诊断种类对齐。
type DiagnosticKind string

const (
	DiagMissingFrontmatter   DiagnosticKind = "missing_frontmatter"
	DiagUnclosedFrontmatter  DiagnosticKind = "unclosed_frontmatter"
	DiagDecoratedKey         DiagnosticKind = "decorated_key"
	DiagUnattributedToolItem DiagnosticKind = "unattributed_tool_item"
	DiagMissingDescription   DiagnosticKind = "missing_description"
	DiagNameMismatch         DiagnosticKind = "name_mismatch"
	DiagEmptyBody            DiagnosticKind = "empty_body"
)

type Diagnostic struct {
	Kind     DiagnosticKind `json:"kind"`
	Severity Severity       `json:"severity"`
	Message  string         `json:"message"`
}

// Skill 是一个技能。
type Skill struct {
	Name         string
	Description  string
	Body         string // 去掉 frontmatter 之后的正文
	Path         string
	AllowedTools []string
	// Unlocks 是渐进式披露的落点:加载这个技能会**解锁**哪些工具。
	// 被门控的工具默认不下发 schema,直到模型加载了声明它的技能。
	Unlocks     []string
	Diagnostics []Diagnostic
}

func (s *Skill) HasError() bool {
	for _, d := range s.Diagnostics {
		if d.Severity == SevError {
			return true
		}
	}
	return false
}

// Set 是技能注册表。
type Set struct {
	skills []Skill
	dir    string
}

// Load 从目录加载全部技能(每个子目录一个 SKILL.md)。
func Load(dir string) (*Set, error) {
	set := &Set{dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil // 目录不存在不是错误(与 Rust 版一致)
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		md := filepath.Join(dir, e.Name(), "SKILL.md")
		info, err := os.Stat(md)
		if err != nil || info.IsDir() {
			continue
		}
		text, err := os.ReadFile(md)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", md, err)
		}
		set.skills = append(set.skills, parseSkill(e.Name(), md, string(text)))
	}
	sort.Slice(set.skills, func(i, j int) bool { return set.skills[i].Name < set.skills[j].Name })
	return set, nil
}

func Empty() *Set { return &Set{} }

func (s *Set) Len() int        { return len(s.skills) }
func (s *Set) Skills() []Skill { return s.skills }
func (s *Set) Dir() string     { return s.dir }

func (s *Set) Names() []string {
	out := make([]string, 0, len(s.skills))
	for _, sk := range s.skills {
		out = append(out, sk.Name)
	}
	return out
}

func (s *Set) Get(name string) *Skill {
	for i := range s.skills {
		if s.skills[i].Name == name {
			return &s.skills[i]
		}
	}
	return nil
}

// UnlocksFor 返回某技能声明的解锁模式。
func (s *Set) UnlocksFor(name string) []string {
	if sk := s.Get(name); sk != nil {
		return sk.Unlocks
	}
	return nil
}

// Unlocks 返回所有技能 unlocks 的并集(工具平面的门控模式)。
func (s *Set) Unlocks() []string {
	var out []string
	for _, sk := range s.skills {
		for _, p := range sk.Unlocks {
			found := false
			for _, x := range out {
				if x == p {
					found = true
					break
				}
			}
			if !found {
				out = append(out, p)
			}
		}
	}
	return out
}

// Body 返回某技能的正文(供 xk__skill_load 按需加载)。
func (s *Set) Body(name string) (string, bool) {
	if sk := s.Get(name); sk != nil {
		return sk.Body, true
	}
	return "", false
}

// HasGating 是否有任何技能声明了 unlocks(决定工具是否需要门控)。
func (s *Set) HasGating() bool {
	for _, sk := range s.skills {
		if len(sk.Unlocks) > 0 {
			return true
		}
	}
	return false
}

func (s *Set) ErrorCount() int {
	n := 0
	for _, sk := range s.skills {
		if sk.HasError() {
			n++
		}
	}
	return n
}

// ─────────────────────────── 渲染 ───────────────────────────

// Payload 按模式渲染注入片段。
func (s *Set) Payload(mode Mode, maxIndexChars, maxFullChars int) string {
	switch mode {
	case ModeIndex:
		return s.indexPayload(maxIndexChars)
	case ModeFull:
		return s.fullPayload(maxFullChars)
	default:
		return ""
	}
}

func (s *Set) indexPayload(maxChars int) string {
	if len(s.skills) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("[local-audit-lab/skills]\n" +
		"The following capabilities are available in this workspace. " +
		"Treat them as established procedures, not suggestions.\n")
	for _, skill := range s.skills {
		line := fmt.Sprintf("- %s: %s\n", skill.Name, oneLine(skill.Description))
		if runeLen(out.String())+runeLen(line) > maxChars {
			out.WriteString("- …(more skills omitted for budget)\n")
			break
		}
		out.WriteString(line)
	}
	return out.String()
}

// fullPayload 把技能正文整篇注入。成本随技能数线性上升,所以有硬上限;
// 被截断的技能会明确标注,免得模型以为看到的是完整规程。
func (s *Set) fullPayload(maxChars int) string {
	base := s.indexPayload(maxChars)
	if base == "" {
		return ""
	}
	var out strings.Builder
	out.WriteString(base)
	out.WriteString("\n[local-audit-lab/skills/full]\n")
	for _, skill := range s.skills {
		section := fmt.Sprintf("\n--- skill: %s ---\n%s\n", skill.Name, strings.TrimRight(skill.Body, " \t\n"))
		if runeLen(out.String())+runeLen(section) > maxChars {
			out.WriteString(fmt.Sprintf("\n--- skill: %s --- (omitted: char budget exhausted)\n", skill.Name))
			continue
		}
		out.WriteString(section)
	}
	return out.String()
}

func runeLen(s string) int { return len([]rune(s)) }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ─────────────────────────── SKILL.md 解析 ───────────────────────────

func parseSkill(dirName, path, text string) Skill {
	skill := Skill{Name: dirName, Path: path}
	lines := strings.Split(text, "\n")

	// frontmatter:首行 --- 到下一个 ---;未闭合则把全文当前言(宽容)。
	bodyStart := 0
	var frontmatter []string
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		closed := false
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				closed = true
				bodyStart = i + 1
				break
			}
			frontmatter = append(frontmatter, lines[i])
		}
		if !closed {
			skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
				Kind: DiagUnclosedFrontmatter, Severity: SevWarning,
				Message: "frontmatter 未闭合,已按全文解析",
			})
			bodyStart = 0
			frontmatter = nil
		}
	} else {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Kind: DiagMissingFrontmatter, Severity: SevWarning,
			Message: "缺少 frontmatter,按目录名作为技能名",
		})
	}

	var name, description *string
	var allowedTools, unlocks []string
	// listKey 记录当前正在收集列表项的 key:YAML 块状列表(key: 换行后跟 - item)
	// 是 agentskills.io 规范的标准写法,不支持它 unlocks 会**静默变空**。
	listKey := ""
	for _, line := range frontmatter {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") && !isDecoratedKeyLine(trimmed) {
			continue // 纯注释,且不打断正在收集的块列表
		}
		if item, ok := listItem(trimmed); ok {
			item = strings.TrimSpace(strings.Trim(stripInlineComment(item), "\"'"))
			if item != "" {
				switch listKey {
				case "allowed-tools":
					allowedTools = append(allowedTools, item)
				case "unlocks":
					unlocks = append(unlocks, item)
				default:
					if looksLikeToolName(item) {
						skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
							Kind: DiagUnattributedToolItem, Severity: SevWarning,
							Message: fmt.Sprintf("列表项 `%s` 没有归属的 key,已忽略", item),
						})
					}
				}
			}
			continue
		}
		key, value, ok := splitKey(trimmed)
		if !ok {
			continue
		}
		cleaned := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(key), "#"))
		if cleaned != strings.TrimSpace(key) {
			skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
				Kind: DiagDecoratedKey, Severity: SevWarning,
				Message: fmt.Sprintf("key `%s` 带了多余修饰,已按 `%s` 解析", key, cleaned),
			})
		}
		value = strings.TrimSpace(strings.Trim(strings.TrimSpace(value), "\"'"))
		switch cleaned {
		case "name":
			v := value
			name = &v
			listKey = ""
		case "description":
			v := value
			description = &v
			listKey = ""
		case "allowed-tools", "allowed_tools":
			allowedTools = strings.Fields(stripInlineComment(value))
			listKey = "allowed-tools"
		case "unlocks", "unlock":
			for _, v := range strings.Fields(stripInlineComment(value)) {
				if v = strings.TrimSuffix(v, ","); v != "" {
					unlocks = append(unlocks, v)
				}
			}
			listKey = "unlocks"
		default:
			listKey = ""
		}
	}

	if name != nil && *name != "" {
		skill.Name = *name
		if *name != dirName {
			skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
				Kind: DiagNameMismatch, Severity: SevWarning,
				Message: fmt.Sprintf("name `%s` 与目录名 `%s` 不一致", *name, dirName),
			})
		}
	}
	if description != nil {
		skill.Description = *description
	} else {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Kind: DiagMissingDescription, Severity: SevError,
			Message: "缺少 description:技能无法被有意义地索引",
		})
	}
	if bodyStart < len(lines) {
		skill.Body = strings.Join(lines[bodyStart:], "\n")
	}
	if strings.TrimSpace(skill.Body) == "" {
		skill.Diagnostics = append(skill.Diagnostics, Diagnostic{
			Kind: DiagEmptyBody, Severity: SevWarning, Message: "技能正文为空",
		})
	}
	skill.AllowedTools = allowedTools
	skill.Unlocks = unlocks
	return skill
}

func listItem(trimmed string) (string, bool) {
	if strings.HasPrefix(trimmed, "-") {
		return strings.TrimSpace(trimmed[1:]), true
	}
	if strings.HasPrefix(trimmed, "*") {
		return strings.TrimSpace(trimmed[1:]), true
	}
	return "", false
}

func splitKey(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	return line[:i], line[i+1:], true
}

// isDecoratedKeyLine:`#` 开头但剥掉修饰后是已知 key(如 `## name: x`)。
func isDecoratedKeyLine(line string) bool {
	rest := strings.TrimSpace(strings.TrimLeft(line, "#"))
	key, _, ok := splitKey(rest)
	if !ok {
		return false
	}
	switch strings.TrimSpace(key) {
	case "name", "description", "allowed-tools", "allowed_tools", "unlocks", "unlock":
		return true
	}
	return false
}

// stripInlineComment 剥掉第一个「空白 + #」之后的内容。
func stripInlineComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		return s[:i]
	}
	return s
}

func looksLikeToolName(s string) bool {
	return strings.HasPrefix(s, "xk__") || (strings.Contains(s, "*") && strings.Contains(s, "__"))
}
