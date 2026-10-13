package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

// 替换+备份模型:有原件→备份后整体替换,Off 还原,删除原件回归;
// 无原件→我们创建,删除时文件移除。
func TestXkCodexInjectReplaceAndRestore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir) // codexAgentsPath 跟 HOME
	agents := filepath.Join(dir, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(agents), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agents, []byte("我自己的 Codex 说明\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetXkCodexInject("injected contract", "a.md", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(agents); string(got) != "injected contract\n" {
		t.Fatalf("替换后=%q", got)
	}
	if bak, _ := os.ReadFile(agents + ".magpie-bak"); string(bak) != "我自己的 Codex 说明\n" {
		t.Fatalf("备份=%q", bak)
	}
	// Off:还原原件,备份保留。
	if err := SetXkCodexInjectEnabled(false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(agents); string(got) != "我自己的 Codex 说明\n" {
		t.Fatalf("Off 后=%q", got)
	}
	if _, err := os.Stat(agents + ".magpie-bak"); err != nil {
		t.Fatal("Off 不应删备份")
	}
	// 删除:原件回归,备份清掉。
	if err := SetXkCodexInjectEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := ClearXkCodexInject(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(agents); string(got) != "我自己的 Codex 说明\n" {
		t.Fatalf("删除后=%q", got)
	}
	if _, err := os.Stat(agents + ".magpie-bak"); !os.IsNotExist(err) {
		t.Fatal("删除后备份应收尾")
	}
	if st := XkCodexInject(); st.Text != "" || st.Enabled {
		t.Fatalf("状态=%+v", st)
	}
}

// 原本没有 AGENTS.md:注入创建文件,删除时移除。
func TestXkCodexInjectCreatesAndRemoves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	agents := filepath.Join(dir, ".codex", "AGENTS.md")
	if err := SetXkCodexInject("contract", "b.md", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(agents); string(got) != "contract\n" {
		t.Fatalf("创建=%q", got)
	}
	if err := ClearXkCodexInject(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agents); !os.IsNotExist(err) {
		t.Fatal("删除后文件应移除")
	}
}
