package main

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
		fmt.Println("usage: magpie xk on|off|status|persona [set <text>|set - <file|clear]")
		return nil
	}
	switch args[0] {
	case "on":
		if err := gateway.SetXkMaster(true); err != nil {
			return err
		}
		fmt.Println("xkeysmith 语义管线:开(semantic=yes 的 provider 立即恢复走管线)")
	case "off":
		if err := gateway.SetXkMaster(false); err != nil {
			return err
		}
		fmt.Println("xkeysmith 语义管线:关(所有请求原样直连,几秒内生效)")
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
	default:
		return fmt.Errorf("unknown xk command %q: use on|off|status|persona", args[0])
	}
	return nil
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
