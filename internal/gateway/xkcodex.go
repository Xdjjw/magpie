package gateway

// Codex 目录注入:把上传的提示词文件**整体替换** ~/.codex/AGENTS.md,
// 原件在第一次替换时备份为 AGENTS.md.magpie-bak;开关 Off 或删除 =
// 从备份还原原件(原本没有这个文件则删掉我们创建的)。
//
// 与 keysmith-switch 同一注入位、同一"替换+备份"模型,但由 magpie 统一
// 管理。人格(网关层)与它(客户端层)各自独立、同时生效。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/settings"
)

const xkCodexInjectFile = "xkeysmith-codex-inject.json"

// codexAgentsPath 是注入目标:~/.codex/AGENTS.md;空串 = 找不到 home。
func codexAgentsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "AGENTS.md")
}

type xkCodexInjectState struct {
	Text        string `json:"text"`
	Source      string `json:"source,omitempty"`
	Enabled     bool   `json:"enabled"`
	HadOriginal bool   `json:"had_original"` // 替换前用户是否已有 AGENTS.md
}

var xkCodexMu sync.Mutex

func xkCodexInjectPath() string {
	return filepath.Join(settings.Dir(), xkCodexInjectFile)
}

// XkCodexInject 读注入状态。
func XkCodexInject() xkCodexInjectState {
	var st xkCodexInjectState
	if data, err := os.ReadFile(xkCodexInjectPath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

// SetXkCodexInject 上传/替换注入文件:整体替换 AGENTS.md,原件首次备份。
func SetXkCodexInject(text, source string, enabled bool) error {
	xkCodexMu.Lock()
	defer xkCodexMu.Unlock()
	old := XkCodexInject()
	st := xkCodexInjectState{Text: strings.TrimSpace(text), Source: source,
		Enabled: enabled && strings.TrimSpace(text) != ""}
	return xkCodexCommit(st, old)
}

// SetXkCodexInjectEnabled 只拨开关:On = 写入注入文件,Off = 还原原件。
func SetXkCodexInjectEnabled(on bool) error {
	xkCodexMu.Lock()
	defer xkCodexMu.Unlock()
	st := XkCodexInject()
	st.Enabled = on && st.Text != ""
	return xkCodexCommit(st, st)
}

// ClearXkCodexInject 删除:还原原件(或删掉我们创建的),状态清空。
func ClearXkCodexInject() error {
	xkCodexMu.Lock()
	defer xkCodexMu.Unlock()
	old := XkCodexInject()
	st := xkCodexInjectState{}
	if path := codexAgentsPath(); path != "" && old.Text != "" && !old.HadOriginal {
		_ = os.Remove(path) // 我们创建的文件,撤下即删
	}
	return xkCodexCommit(st, old)
}

// xkCodexCommit 落状态 + 落 AGENTS.md。替换前第一次做原件备份;
// 撤下时从备份还原。
func xkCodexCommit(st, old xkCodexInjectState) error {
	path := codexAgentsPath()
	if path != "" {
		bak := path + ".magpie-bak"
		if st.Enabled && st.Text != "" {
			if _, err := os.Stat(bak); os.IsNotExist(err) {
				if orig, rerr := os.ReadFile(path); rerr == nil {
					st.HadOriginal = true
					if werr := os.WriteFile(bak, orig, 0o644); werr != nil {
						return werr
					}
				} else {
					st.HadOriginal = false
				}
			} else {
				st.HadOriginal = true
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(st.Text+"\n"), 0o644); err != nil {
				return err
			}
		} else if old.Text != "" || old.HadOriginal {
			// 撤下:有备份还原原件(备份只在还有注入内容时保留,
			// Clear 之后再来一次上传会重新备份当时在场的内容)。
			if _, err := os.Stat(bak); err == nil {
				if st.Text == "" {
					_ = os.Rename(bak, path) // 删除:原件回归,备份功成身退
				} else {
					orig, rerr := os.ReadFile(bak)
					if rerr == nil {
						_ = os.WriteFile(path, orig, 0o644) // 开关 Off:还原,留备份
					}
				}
			}
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tpath := xkCodexInjectPath()
	if err := os.MkdirAll(filepath.Dir(tpath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(tpath), ".xkeysmith-codex-*")
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
	return os.Rename(tmp.Name(), tpath)
}
