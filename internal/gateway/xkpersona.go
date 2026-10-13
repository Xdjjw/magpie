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
	// Source 是文本的来源(CLI 上传时是可回读的绝对路径,网页上传时是
	// 文件名标签)。一键关闭时,可读的源文件优先于快照还原。
	Source string `json:"source,omitempty"`
	// Enabled 是独立的启用位:文本保留、开关决定是否注入。nil =
	// 旧文件没有这个字段,按「有文本即启用」推导,行为与之前一致。
	Enabled *bool `json:"enabled,omitempty"`
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

func xkPersonaStateRead() xkPersonaState {
	var st xkPersonaState
	if data, err := os.ReadFile(xkPersonaPath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

// XkPersonaEnabled 报告人格是否注入:显式开关优先,旧文件按文本
// 非空推导。
func XkPersonaEnabled() bool {
	st := xkPersonaStateRead()
	if st.Enabled != nil {
		return *st.Enabled
	}
	return strings.TrimSpace(st.Text) != ""
}

// SetXkPersonaEnabled 拨启用开关,文本不动。
func SetXkPersonaEnabled(on bool) error {
	st := xkPersonaStateRead()
	st.Enabled = &on
	return xkPersonaStateWrite(st)
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

// SetXkPersona 写人格(原子写,首尾空白去掉,超长截断);空串即清除文本。
// 启用位不动——那是开关的事。CLI 的 `magpie xk persona set|clear` 调它。
func SetXkPersona(text string) error {
	return SetXkPersonaSource(text, "")
}

// SetXkPersonaSource 写人格并记住来源。
func SetXkPersonaSource(text, source string) error {
	text = strings.TrimSpace(text)
	if n := utf8.RuneCountInString(text); n > xkPersonaMax {
		text = string([]rune(text)[:xkPersonaMax])
	}
	st := xkPersonaStateRead()
	st.Text, st.Source = text, strings.TrimSpace(source)
	return xkPersonaStateWrite(st)
}

// XkPersonaSource 返回人格的来源标注(可能只是文件名)。
func XkPersonaSource() string {
	return xkPersonaStateRead().Source
}

// xkPersonaStateWrite 原子写状态并使读缓存失效。
func xkPersonaStateWrite(st xkPersonaState) error {
	path := xkPersonaPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
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

// XkPersonaInit 首次运行时装上默认人格。总开关默认开、人格却默认空,
// 全新安装直到有人拨一次开关才会有注入——「初始化就默认注入」指的
// 是这里。只在人格文件从未存在过时写:`persona clear` 落下的空文件
// 不会被覆盖,对照实验的语义不变。
func XkPersonaInit() {
	if _, err := os.Stat(xkPersonaPath()); err == nil {
		return
	}
	_ = SetXkPersona(xkDefaultPersona)
}

// xkPersonaInjection 是随请求上行的注入块:带一个可检索的标记头,
// 审计 JSONL 的 RequestBody 里能一眼认出哪段是人格。
func xkPersonaInjection() string {
	if !XkPersonaEnabled() {
		return ""
	}
	persona := strings.TrimSpace(XkPersona())
	if persona == "" {
		return ""
	}
	return "[session-persona]\n" + persona
}
