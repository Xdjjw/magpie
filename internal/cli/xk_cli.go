package cli

// `magpie xk` —— 语义管线总开关 + 入口人格。
//
// 开关管全部:关掉后所有请求原样直连,per-provider 的 semantic
// 标记被忽略。拨动几秒内生效,不用重启。状态文件在 magpie 配置目录。
//
// persona 是入口注入的 system 人格/上下文框架:`magpie xk persona set -`
// 从标准输入读多行文本,`clear` 清除。它从 L0 起随请求上行。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/gateway"
)

func xkCmd(args []string) error {
	// 与 providerCmd 同约定:args[0] 是命令名本身,剥掉。
	if len(args) > 0 && args[0] == "xk" {
		args = args[1:]
	}
	gateway.XkPersonaInit()
	if len(args) == 0 {
		state := "on"
		if !gateway.XkMasterOn() {
			state = "off"
		}
		fmt.Println("xkeysmith semantic pipeline:", state)
		fmt.Println("usage: magpie xk on|off|status|persona [set <text>|set - <file>|clear|show]|stats [N]")
		return nil
	}
	switch args[0] {
	case "on":
		installed, err := gateway.SetXkPower(true)
		if err != nil {
			return err
		}
		if installed {
			fmt.Println("xkeysmith 语义管线:开(人格为空,已自动部署推荐模板;手写人格不会被覆盖)")
		} else {
			fmt.Println("xkeysmith 语义管线:开(保留现有人格)")
		}
	case "off":
		_, err := gateway.SetXkPower(false)
		if err != nil {
			return err
		}
		fmt.Println("xkeysmith 语义管线:关(请求原样直连,人格已还原到开启前;几秒内生效)")
	case "status":
		if gateway.XkMasterOn() {
			fmt.Println("on")
		} else {
			fmt.Println("off")
		}
		if p := strings.TrimSpace(gateway.XkPersona()); p != "" {
			fmt.Printf("persona: %d chars\n", len(p))
		} else {
			fmt.Println("persona: off")
		}
	case "persona":
		return xkPersonaCmd(args[1:])
	case "stats":
		return xkStatsCmd(args[1:])
	case "codex":
		return xkCodexCmd(args[1:])
	default:
		return fmt.Errorf("unknown xk command %q: use on|off|status|persona|stats", args[0])
	}
	return nil
}

// `magpie xk stats [N]`:最近 N 条阶梯尝试(默认 500)的拒绝率与分层分布。
// persona 前后各跑一段,这个数字一对比就是调参依据。
func xkStatsCmd(args []string) error {
	last := 500
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 {
			return fmt.Errorf("usage: magpie xk stats [N] (N>=1)")
		}
		last = n
	}
	s, err := gateway.XkStats(last)
	if err != nil {
		return err
	}
	if s.Attempts == 0 {
		fmt.Println("no ladder attempts recorded yet(审计目录还没有数据)")
		return nil
	}
	fmt.Printf("attempts %d · requests %d · refused %d (%.1f%% of attempts, %.1f%% of requests hit a refusal)\n",
		s.Attempts, s.Requests, s.Refused, s.RefusalRate*100, s.RequestRefusalRate*100)
	fmt.Printf("decisions: accepted %d · escalated %d · stopped %d\n", s.Accepted, s.Escalated, s.Stopped)
	fmt.Println("layer   attempts  passed  uncertain  refused  refuse%")
	for _, row := range s.Layers {
		fmt.Printf("%-6s %8d %7d %10d %8d %7.1f\n", row.Layer, row.Attempts, row.Passed, row.Uncertain, row.Refused, row.RefusePct*100)
	}
	if len(s.Families) > 0 {
		fmt.Println("not-passed families:")
		for _, k := range sortedByCount(s.Families) {
			fmt.Printf("  %-24s %d\n", k, s.Families[k])
		}
	}
	if len(s.Signals) > 0 {
		fmt.Println("top not-passed signals:")
		for i, k := range sortedByCount(s.Signals) {
			if i >= 5 {
				break
			}
			fmt.Printf("  %-32s %d\n", k, s.Signals[k])
		}
	}
	return nil
}

func sortedByCount(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

func xkPersonaCmd(args []string) error {
	if len(args) == 0 || args[0] == "show" {
		p := gateway.XkPersona()
		if p == "" {
			fmt.Println("persona: off(未设置;magpie xk persona set - < file 写入)")
			return nil
		}
		fmt.Println(p)
		return nil
	}
	switch args[0] {
	case "set":
		var text, source string
		switch {
		case len(args) >= 3 && args[1] == "-":
			// `set - <file>`:从文件读;记住绝对路径,一键关闭时
			// 以磁盘上的这份为准还原。
			data, err := os.ReadFile(args[2])
			if err != nil {
				return err
			}
			text = string(data)
			if abs, aerr := filepath.Abs(args[2]); aerr == nil {
				source = abs
			}
		case len(args) >= 2 && args[1] == "-":
			// `set -`:标准输入。
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			text = string(data)
		case len(args) >= 2:
			text = strings.Join(args[1:], " ")
		default:
			return fmt.Errorf("usage: magpie xk persona set <text>|set -|set - <file>")
		}
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("persona 文本为空;要清除用 magpie xk persona clear")
		}
		if err := gateway.SetXkPersonaSource(text, source); err != nil {
			return err
		}
		if err := gateway.SetXkPersonaEnabled(true); err != nil {
			return err
		}
		fmt.Printf("persona: 已设置并启用(%d 字符,来自 %s;2 秒内生效)\n", len(strings.TrimSpace(text)), source)
	case "clear":
		if err := gateway.SetXkPersona(""); err != nil {
			return err
		}
		fmt.Println("persona: 已清除(请求恢复原样)")
	case "on", "off":
		if err := gateway.SetXkPersonaEnabled(args[0] == "on"); err != nil {
			return err
		}
		fmt.Println("persona:", args[0])
	default:
		return fmt.Errorf("unknown persona command %q: use show|set|clear|on|off", args[0])
	}
	return nil
}

// `magpie xk codex`:整体替换 ~/.codex/AGENTS.md(原件自动备份),
// 与人格各自独立、同时生效(客户端层 + 网关层)。
func xkCodexCmd(args []string) error {
	if len(args) == 0 || args[0] == "show" {
		st := gateway.XkCodexInject()
		if st.Text == "" {
			fmt.Println("codex inject: off(未上传;magpie xk codex set - < file)")
			return nil
		}
		fmt.Printf("codex inject: %s(%d 字符,源:%s)\n", map[bool]string{true: "on", false: "off"}[st.Enabled], len(st.Text), st.Source)
		return nil
	}
	switch args[0] {
	case "set":
		if len(args) < 3 || args[1] != "-" {
			return fmt.Errorf("usage: magpie xk codex set - <file>")
		}
		data, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		source := args[2]
		if abs, aerr := filepath.Abs(args[2]); aerr == nil {
			source = abs
		}
		if err := gateway.SetXkCodexInject(string(data), source, true); err != nil {
			return err
		}
		fmt.Printf("codex inject: 已替换 ~/.codex/AGENTS.md(%d 字符,原件备份为 AGENTS.md.magpie-bak)\n", len(data))
	case "on", "off":
		if err := gateway.SetXkCodexInjectEnabled(args[0] == "on"); err != nil {
			return err
		}
		fmt.Println("codex inject:", args[0])
	case "clear":
		if err := gateway.ClearXkCodexInject(); err != nil {
			return err
		}
		fmt.Println("codex inject: 已删除,原件已还原")
	default:
		return fmt.Errorf("unknown codex command %q: use show|set|on|off|clear", args[0])
	}
	return nil
}
