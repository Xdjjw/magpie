// Package rewrite is the L0-L4 rewrite ladder, ported from xkeysmith's
// xp-rewrite (Rust). Stage ② of the migration covers L1-L3 (deterministic
// text transforms); L4 (placeholder vault + streaming restore) lands in
// stage ③. The Rust crate stays the spec and the test oracle.
//
// 层是**累积**的:attempt(L3) = L0 + L1 + L2 + L3 依次作用在同一份请求上。
package rewrite

import (
	"regexp"
	"strings"
)

// mapOutsideCode 把文本按「可改写 / 受保护」切分,只对可改写部分应用 f。
//
// L2 的关键工程细节:**正则改写必须跳过代码**。否则 `crack` 会把
// `fn crack_target()` 改成 `fn compatibility_patching_target()`,
// 直接毁掉 Codex 这类客户端真正需要的东西。保护三类区间:
// fenced code block、行内代码、URL(对 L2 而言 URL 被改写链接就废了)。
func mapOutsideCode(input string, f func(string) string) string {
	var out strings.Builder
	out.Grow(len(input) + 16)
	var fenced string // 非空 = 正在围栏内
	inFence := false

	for _, line := range splitKeepNewline(input) {
		trimmed := strings.TrimLeft(line, " \t")
		if inFence {
			out.WriteString(line)
			if strings.HasPrefix(trimmed, fenced) {
				inFence = false
			}
			continue
		}
		if delim, ok := fenceDelimiter(trimmed); ok {
			fenced, inFence = delim, true
			out.WriteString(line)
			continue
		}
		out.WriteString(mapOutsideInlineCode(line, f))
	}
	return out.String()
}

// mapOutsideCodeAndURLs 同上,但额外把 URL 视为受保护区间。
func mapOutsideCodeAndURLs(input string, f func(string) string) string {
	return mapOutsideCode(input, func(seg string) string { return maskURLs(seg, f) })
}

var urlRe = regexp.MustCompile(`(?i)\b(?:https?|wss?|ftp)://[^\s<>()\[\]{}"']+`)

func maskURLs(seg string, f func(string) string) string {
	locs := urlRe.FindAllStringIndex(seg, -1)
	if locs == nil {
		return f(seg)
	}
	var masked strings.Builder
	var stash []string
	last := 0
	for _, m := range locs {
		masked.WriteString(seg[last:m[0]])
		masked.WriteString("\x00")
		masked.WriteString(itoa(len(stash)))
		masked.WriteString("\x00")
		stash = append(stash, seg[m[0]:m[1]])
		last = m[1]
	}
	masked.WriteString(seg[last:])

	rewritten := f(masked.String())

	// 还原被 stash 的 URL;未闭合/无法解析的 \x00idx\x00 原样保留。
	var restored strings.Builder
	runes := []rune(rewritten)
	for i := 0; i < len(runes); {
		if runes[i] != '\x00' {
			restored.WriteRune(runes[i])
			i++
			continue
		}
		j := i + 1
		var idx strings.Builder
		for j < len(runes) && runes[j] != '\x00' {
			idx.WriteRune(runes[j])
			j++
		}
		if j >= len(runes) {
			// 未闭合:原样吐出剩余
			restored.WriteString(string(runes[i:]))
			break
		}
		n, ok := atoi(idx.String())
		if ok && n < len(stash) {
			restored.WriteString(stash[n])
		} else {
			restored.WriteRune('\x00')
			restored.WriteString(idx.String())
			restored.WriteRune('\x00')
		}
		i = j + 1
	}
	return restored.String()
}

func mapOutsideInlineCode(line string, f func(string) string) string {
	var out strings.Builder
	rest := line
	for {
		tick := strings.IndexByte(rest, '`')
		if tick < 0 {
			out.WriteString(f(rest))
			return out.String()
		}
		out.WriteString(f(rest[:tick]))
		fromTick := rest[tick:]
		ticks := 0
		for ticks < len(fromTick) && fromTick[ticks] == '`' {
			ticks++
		}
		delim := strings.Repeat("`", ticks)
		afterOpen := fromTick[ticks:]
		closeIdx := strings.Index(afterOpen, delim)
		if closeIdx < 0 {
			// 未闭合的行内代码:当普通文本处理,避免吞掉整行
			out.WriteString(f(fromTick))
			return out.String()
		}
		// 连同分隔符一起原样保留
		out.WriteString(fromTick[:ticks])
		out.WriteString(afterOpen[:closeIdx+ticks])
		rest = afterOpen[closeIdx+ticks:]
	}
}

// fenceDelimiter 识别 fence 起始行(```lang / ~~~lang),返回围栏定界符。
func fenceDelimiter(trimmedLine string) (string, bool) {
	if trimmedLine == "" {
		return "", false
	}
	first := trimmedLine[0]
	if first != '`' && first != '~' {
		return "", false
	}
	n := 0
	for n < len(trimmedLine) && trimmedLine[n] == first {
		n++
	}
	if n < 3 {
		return "", false
	}
	return trimmedLine[:n], true
}

// splitKeepNewline 按行切分但保留行尾换行符,保证拼接后字节完全一致。
func splitKeepNewline(input string) []string {
	if input == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(input); i++ {
		if input[i] == '\n' {
			out = append(out, input[start:i+1])
			start = i + 1
		}
	}
	if start < len(input) {
		out = append(out, input[start:])
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
