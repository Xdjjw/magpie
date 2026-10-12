package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

// stats 聚合:fixture 审计行进 JSONL,验证层分布、两种拒绝率、
// 决策计数与非 passed 家族聚合;工具回路等无 verdict 的行被跳过。
func TestXkStatsAggregatesAuditRecords(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(settings.Dir(), "xkeysmith", "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(trace string, layer int, outcome, decision string, families ...string) string {
		b, _ := json.Marshal(map[string]any{
			"trace": trace, "layer": layer, "decision": decision,
			"verdict": map[string]any{"outcome": outcome, "families": families, "signals": []string{"refusal_field"}},
		})
		return string(b)
	}
	rows := []string{
		line("11111111-1111-1111-1111-111111111111", 0, "refused", "escalate:L1", "structured"),
		line("11111111-1111-1111-1111-111111111111", 1, "passed", "accept", "structured"), // passed 层的家族不计入
		line("22222222-2222-2222-2222-222222222222", 0, "passed", "accept"),
		line("33333333-3333-3333-3333-333333333333", 0, "refused", "escalate:L1", "lexical"),
		line("33333333-3333-3333-3333-333333333333", 1, "refused", "stop:ladder_exhausted", "lexical"),
		`{"trace":"44444444-4444-4444-4444-444444444444","layer":0,"decision":"tool:continue"}`, // 无 verdict:跳过
	}
	if err := os.WriteFile(filepath.Join(dir, "audit-20261012-1.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := XkStats(0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Attempts != 5 || s.Requests != 3 {
		t.Fatalf("attempts=%d requests=%d", s.Attempts, s.Requests)
	}
	if s.Refused != 3 || s.Passed != 2 || s.Uncertain != 0 {
		t.Fatalf("refused=%d passed=%d uncertain=%d", s.Refused, s.Passed, s.Uncertain)
	}
	// 3/5 尝试被拒;3 个请求里 2 个(aaaa、cccc)至少一层被拒。
	if s.RefusalRate != 0.6 || s.RequestRefusalRate*100 < 66.6 || s.RequestRefusalRate*100 > 66.7 {
		t.Fatalf("attempt rate=%v request rate=%v", s.RefusalRate, s.RequestRefusalRate)
	}
	if s.Accepted != 2 || s.Escalated != 2 || s.Stopped != 1 {
		t.Fatalf("accepted=%d escalated=%d stopped=%d", s.Accepted, s.Escalated, s.Stopped)
	}
	if s.Families["lexical"] != 2 || s.Families["structured"] != 1 {
		t.Fatalf("families=%v", s.Families)
	}
	if s.Signals["refusal_field"] != 3 {
		t.Fatalf("signals=%v", s.Signals)
	}
	// 层分布:L0 三次两拒,L1 两次一拒。
	for _, row := range s.Layers {
		if row.Layer == "L0" && (row.Attempts != 3 || row.Refused != 2) {
			t.Fatalf("L0 row=%+v", row)
		}
		if row.Layer == "L1" && (row.Attempts != 2 || row.Refused != 1) {
			t.Fatalf("L1 row=%+v", row)
		}
	}
}

// 空审计目录:零值摘要可用,不报错。
func TestXkStatsEmptyDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, err := XkStats(500)
	if err != nil || s.Attempts != 0 {
		t.Fatalf("s=%+v err=%v", s, err)
	}
}
