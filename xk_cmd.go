package main

// `magpie xk` —— 语义管线的**总开关**。
//
// 一个开关管全部:关掉后所有请求原样直连,per-provider 的 semantic
// 标记被忽略。拨动几秒内生效,不用重启。状态文件在 magpie 配置目录。

import (
	"fmt"

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
		fmt.Println("usage: magpie xk on|off|status")
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
	default:
		return fmt.Errorf("unknown xk command %q: use on|off|status", args[0])
	}
	return nil
}
