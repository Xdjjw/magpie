package gateway

// 语义管线的**入口人格**:一段用户自定义的 system 提示词,在阶梯入口
// (L0 起)就随请求上行,不等第一次被拒才由 L1 补上下文。
//
// 为什么在入口而不是 L1:阶梯是「先直通、被拒了再加码」的被动结构,
// 每个会被拒的请求都要先白付一次上游调用。把人格/上下文框架放在入口,
// 大量关键词触发的误杀在第一次调用就不会发生,阶梯只在真正需要时才升。
//
// 约定与总开关一致:小 JSON 文件(原子写)+ 2 秒读缓存,热路径零 IO;
// 空文本 = 关(请求原样,语义管线行为与没有人格时完全一致)。人格是
// **真实的工作上下文框架**(授权的安全研究、内部工程环境),不是让
// 上游无视其政策的指令——那类内容在这里没有位置。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/settings"
)

const xkPersonaFile = "xkeysmith-persona.json"

// xkPersonaMax 是人格文本的上限:它是框架不是小说,超长的部分截掉,
// 提示词膨胀会挤占请求本身的上下文。
const xkPersonaMax = 8192

type xkPersonaState struct {
	Text string `json:"text"`
}

func xkPersonaPath() string {
	return filepath.Join(settings.Dir(), xkPersonaFile)
}

// xkPersonaCache 与总开关同款:2 秒 TTL,写后立即失效。
var xkPersonaCache struct {
	mu      sync.Mutex
	current atomic.Pointer[xkPersonaSnapshot]
}

type xkPersonaSnapshot struct {
	text    string
	expires int64
	path    string
}

// XkPersona 返回当前人格文本;空串 = 未设置。
func XkPersona() string {
	now := time.Now().UnixMilli()
	path := xkPersonaPath()
	if s := xkPersonaCache.current.Load(); s != nil && s.path == path && now < s.expires {
		return s.text
	}
	xkPersonaCache.mu.Lock()
	defer xkPersonaCache.mu.Unlock()
	if s := xkPersonaCache.current.Load(); s != nil && s.path == path && now < s.expires {
		return s.text
	}
	text := ""
	if data, err := os.ReadFile(path); err == nil {
		var state xkPersonaState
		if json.Unmarshal(data, &state) == nil {
			text = state.Text
		}
	}
	xkPersonaCache.current.Store(&xkPersonaSnapshot{text: text, expires: now + 2000, path: path})
	return text
}

// SetXkPersona 写人格(原子写,首尾空白去掉,超长截断);空串即清除。
// CLI 的 `magpie xk persona set|clear` 调它。
func SetXkPersona(text string) error {
	text = strings.TrimSpace(text)
	if n := utf8.RuneCountInString(text); n > xkPersonaMax {
		text = string([]rune(text)[:xkPersonaMax])
	}
	path := xkPersonaPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(xkPersonaState{Text: text}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".xkeysmith-persona-*")
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	xkPersonaCache.current.Store(nil)
	return nil
}

// xkPersonaInjection 是随请求上行的注入块:带一个可检索的标记头,
// 审计 JSONL 的 RequestBody 里能一眼认出哪段是人格。
func xkPersonaInjection() string {
	persona := strings.TrimSpace(XkPersona())
	if persona == "" {
		return ""
	}
	return "[session-persona]\n" + persona
}
