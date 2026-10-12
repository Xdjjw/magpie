package gateway

import (
	"strings"
	"testing"
)

// 一键开关:开 = 管线开 + 空人格自动装推荐模板 + 快照;
// 关 = 管线关 + 人格还原到开启前。
func TestXkPowerBundlesMasterAndPersona(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SetXkMaster(false); err != nil {
		t.Fatal(err)
	}
	if err := SetXkPersona(""); err != nil {
		t.Fatal(err)
	}
	// 开:空人格 → 自动装推荐模板,快照记空。
	installed, err := SetXkPower(true)
	if err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Fatal("空人格开启时必须报告已装模板")
	}
	if !XkMasterOn() {
		t.Fatal("开后总开关必须是开")
	}
	if got := XkPersona(); strings.TrimSpace(got) == "" || !strings.Contains(got, "authorized systems-engineering workspace") {
		t.Fatalf("空人格没有被自动装上推荐模板: %d chars", len(got))
	}
	// 手写一段,再关:还原的是开启前的空,不是手写段。
	if err := SetXkPersona("manual override"); err != nil {
		t.Fatal(err)
	}
	if _, err := SetXkPower(false); err != nil {
		t.Fatal(err)
	}
	if XkMasterOn() || XkPersona() != "" {
		t.Fatalf("关后 master=%v persona=%q,应还原到开启前(关/空)", XkMasterOn(), XkPersona())
	}
}

// 开着的状态下手写的人格:再「开」不覆盖;用户显式保留的人格优先于模板。
func TestXkPowerKeepsHandwrittenPersona(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SetXkPersona("my own lab context"); err != nil {
		t.Fatal(err)
	}
	installed, err := SetXkPower(true)
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Fatal("手写人格在场时不应装模板")
	}
	if XkPersona() != "my own lab context" {
		t.Fatalf("手写人格被模板覆盖: %q", XkPersona())
	}
	// 幂等:再拨一次开,快照不重写、人格不动,也不装模板。
	if installed, e := SetXkPower(true); e != nil || installed {
		t.Fatalf("二次开启: installed=%v err=%v", installed, e)
	}
	if err := SetXkPersona("tuned while on"); err != nil {
		t.Fatal(err)
	}
	if _, err := SetXkPower(false); err != nil {
		t.Fatal(err)
	}
	// 还原到「第一次开之前」的快照,而不是开着时的临时修改。
	if XkPersona() != "my own lab context" || XkMasterOn() {
		t.Fatalf("关后 persona=%q master=%v", XkPersona(), XkMasterOn())
	}
}
