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
		var text string
		switch {
		case len(args) >= 3 && args[1] == "-":
			// `set - <file`:从文件读,多行人格不用塞进 argv。
			data, err := os.ReadFile(args[2])
			if err != nil {
				return err
			}
			text = string(data)
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
		if err := gateway.SetXkPersona(text); err != nil {
			return err
		}
		fmt.Printf("persona: 已设置(%d 字符,2 秒内生效;入口注入,所有层生效)\n", len(strings.TrimSpace(text)))
	case "clear":
		if err := gateway.SetXkPersona(""); err != nil {
			return err
		}
		fmt.Println("persona: 已清除(请求恢复原样)")
	default:
		return fmt.Errorf("unknown persona command %q: use show|set|clear", args[0])
	}
	return nil
}
