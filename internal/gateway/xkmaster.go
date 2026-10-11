package gateway

// 语义管线的**总开关**。
//
// 与 per-provider 的 semantic=yes 不同,这是一个**整体**开关:
//   - 开(默认):semantic=yes 的 provider 走语义管线;
//   - 关:**所有**请求原样直连,per-provider 的标记被忽略——
//     管线异常时一拨即断,不用逐个改 provider、不用重启。
//
// 状态落在一个小 JSON 文件里(原子写),请求时带 2 秒缓存读取:
// 拨动开关后最多 2 秒生效,热路径零文件 IO。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

const xkMasterFile = "xkeysmith-master.json"

// 默认开:开关文件不存在 = 语义管线启用(与「迁移完成即生效」一致)。
var xkMasterDefault = true

type xkMasterState struct {
	Enabled bool `json:"enabled"`
}

func xkMasterPath() string {
	return filepath.Join(settings.Dir(), xkMasterFile)
}

// xkMasterCache 是带 2 秒 TTL 的读缓存:热路径不打盘。
var xkMasterCache struct {
	mu      sync.Mutex
	value   bool
	expires atomic.Int64 // unix ms
}

// XkMasterOn 报告总开关当前状态(默认开)。
func XkMasterOn() bool { return xkMasterEnabled() }

func xkMasterEnabled() bool {
	now := time.Now().UnixMilli()
	if now < xkMasterCache.expires.Load() {
		return xkMasterCache.value
	}
	xkMasterCache.mu.Lock()
	defer xkMasterCache.mu.Unlock()
	if now < xkMasterCache.expires.Load() {
		return xkMasterCache.value
	}
	enabled := xkMasterDefault
	if data, err := os.ReadFile(xkMasterPath()); err == nil {
		var state xkMasterState
		if json.Unmarshal(data, &state) == nil {
			enabled = state.Enabled
		}
	}
	xkMasterCache.value = enabled
	xkMasterCache.expires.Store(now + 2000)
	return enabled
}

// SetXkMaster 写总开关(原子写)。CLI 的 `magpie xk on|off` 调它。
func SetXkMaster(on bool) error {
	path := xkMasterPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(xkMasterState{Enabled: on}, "", "  ")
	if err != nil {
		return err
	}
	// 原子写:临时文件 + rename,半行状态不会被当成开关。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// 立刻失效缓存:拨了开关马上生效。
	xkMasterCache.expires.Store(0)
	return nil
}
