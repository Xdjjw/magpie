package gateway

// 一键开关(`magpie xk on|off`、托盘与设置页的同一个开关)。
//
// 语义管线不是一个动作是一组:总开关、入口人格的部署与还原。拆成
// 三个命令是给调试用的;日常只该有一个开关:
//
//   开  = 总开关开 + 人格为空时自动部署内置推荐模板(手写的人格
//         永远优先,不覆盖) + 开启前的人格自动快照;
//   关  = 总开关关 + 人格还原到开启前的样子(空则清除)。
//
// 快照落在自己的小 JSON 文件里(原子写):再次「开」不重复快照,
// 「关」精确还原。只动 magpie 自己的状态,不碰任何本地 agent 的
// 提示词文件——网关注入已经覆盖了那些流量,改它们的文件是另一件事。

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/settings"
)

//go:embed xkpersona_default.txt
var xkDefaultPersona string

// XkDefaultPersona 是内置的推荐人格(docs/xk-persona.txt 的嵌入副本,
// 两处同步修改)。
func XkDefaultPersona() string { return xkDefaultPersona }

const xkPowerFile = "xkeysmith-power.json"

type xkPowerState struct {
	On           bool   `json:"on"`
	Saved        string `json:"saved_persona"`
	SavedEnabled bool   `json:"saved_persona_enabled"`
}

func xkPowerPath() string {
	return filepath.Join(settings.Dir(), xkPowerFile)
}

// xkPowerMu 串行化读改写:CLI、托盘、设置页可能同时拨这个开关。
var xkPowerMu sync.Mutex

func xkPowerRead() xkPowerState {
	var st xkPowerState
	if data, err := os.ReadFile(xkPowerPath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func xkPowerWrite(st xkPowerState) error {
	path := xkPowerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".xkeysmith-power-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// SetXkPower 是那个一键开关:CLI 的 `magpie xk on|off`、托盘菜单和
// 设置页的开关都调它。返回值说这次「开」有没有装上推荐模板。
func SetXkPower(on bool) (bool, error) {
	xkPowerMu.Lock()
	defer xkPowerMu.Unlock()
	st := xkPowerRead()
	if on {
		if !st.On {
			// 第一次拨到开:快照当前人格,空则装推荐模板。
			st.On, st.Saved, st.SavedEnabled = true, XkPersona(), XkPersonaEnabled()
			if err := xkPowerWrite(st); err != nil {
				return false, err
			}
			if strings.TrimSpace(st.Saved) == "" {
				if err := SetXkPersona(xkDefaultPersona); err != nil {
					return false, err
				}
				return true, SetXkMaster(true)
			}
		}
		return false, SetXkMaster(true)
	}
	if st.On {
		// 从开拨到关:还原开启前的人格。
		st.On = false
		if err := xkPowerWrite(st); err != nil {
			return false, err
		}
		if err := SetXkPersona(st.Saved); err != nil {
			return false, err
		}
	}
	return false, SetXkMaster(false)
}
