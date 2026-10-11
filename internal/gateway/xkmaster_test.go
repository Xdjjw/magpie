package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

// 总开关:默认开;写 off 后 2 秒缓存窗口内生效;写回 on 恢复。
func TestXkMasterDefaultOn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if !XkMasterOn() {
		t.Fatal("默认必须是开(开关文件不存在 = 启用)")
	}
}

func TestXkMasterOffThenOn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SetXkMaster(false); err != nil {
		t.Fatal(err)
	}
	if XkMasterOn() {
		t.Fatal("off 之后必须是关")
	}
	// 状态文件确实落盘了
	data, err := os.ReadFile(filepath.Join(settings.Dir(), "xkeysmith-master.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"enabled": false`) {
		t.Fatalf("json=%s", data)
	}
	if err := SetXkMaster(true); err != nil {
		t.Fatal(err)
	}
	if !XkMasterOn() {
		t.Fatal("on 之后必须恢复")
	}
}

func TestXkMasterCorruptFileFallsBackToOn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(settings.Dir(), "xkeysmith-master.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	// 半行(进程被杀):宁可放行也不静默改变行为
	if err := os.WriteFile(path, []byte(`{"enabled": fal`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !XkMasterOn() {
		t.Fatal("损坏的状态文件必须回退到默认(开)")
	}
}
