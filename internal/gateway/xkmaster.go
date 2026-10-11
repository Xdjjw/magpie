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
	current atomic.Pointer[xkMasterSnapshot]
}

type xkMasterSnapshot struct {
	enabled bool
	expires int64
	path    string
}

// XkMasterOn 报告总开关当前状态(默认开)。
func XkMasterOn() bool { return xkMasterEnabled() }

func xkMasterEnabled() bool {
	now := time.Now().UnixMilli()
	path := xkMasterPath()
	if snapshot := xkMasterCache.current.Load(); snapshot != nil && snapshot.path == path && now < snapshot.expires {
		return snapshot.enabled
	}
	xkMasterCache.mu.Lock()
	defer xkMasterCache.mu.Unlock()
	if snapshot := xkMasterCache.current.Load(); snapshot != nil && snapshot.path == path && now < snapshot.expires {
		return snapshot.enabled
	}
	enabled := xkMasterDefault
	if data, err := os.ReadFile(path); err == nil {
		var state xkMasterState
		if json.Unmarshal(data, &state) == nil {
			enabled = state.Enabled
		}
	}
	xkMasterCache.current.Store(&xkMasterSnapshot{enabled: enabled, expires: now + 2000, path: path})
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".xkeysmith-master-*")
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
	// 立刻失效缓存:拨了开关马上生效。
	xkMasterCache.current.Store(nil)
	return nil
}
