package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, name, body string) {
	t.Helper()
	d := filepath.Join(root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParsesSpecCompliantSkill(t *testing.T) {
	root := t.TempDir()
	write(t, root, "api-recon", "---\nname: api-recon\ndescription: 收集网站API接口时调用该skill。\n---\n\n# API Recon\n\n正文内容\n")
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 1 {
		t.Fatalf("len=%d", set.Len())
	}
	s := set.Skills()[0]
	if s.Name != "api-recon" || !strings.Contains(s.Description, "收集") || !strings.HasPrefix(s.Body, "\n# API Recon") {
		t.Fatalf("skill=%+v", s)
	}
	if s.HasError() {
		t.Fatalf("diag=%+v", s.Diagnostics)
	}
}

func TestBlockStyleListsAreParsed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "rev", "---\nname: rev\ndescription: d\nallowed-tools:\n  - xk__skill_load\n  - xk__mcp__ida__decompile_function\nunlocks:\n  - xk__mcp__ida__*\n---\nbody\n")
	set, _ := Load(root)
	s := set.Skills()[0]
	if len(s.AllowedTools) != 2 || s.AllowedTools[1] != "xk__mcp__ida__decompile_function" {
		t.Fatalf("allowed=%v", s.AllowedTools)
	}
	if len(s.Unlocks) != 1 || s.Unlocks[0] != "xk__mcp__ida__*" {
		t.Fatalf("unlocks=%v", s.Unlocks)
	}
	if !set.HasGating() {
		t.Fatal("块状 unlocks 必须参与门控")
	}
}

func TestCommentWithColonDoesNotTruncate(t *testing.T) {
	root := t.TempDir()
	write(t, root, "c", "---\nname: c\ndescription: d\nunlocks:\n  - xk__mcp__ida__*\n  # 备注: ghidra 视情况\n  - xk__mcp__ghidra__*\n---\nb\n")
	set, _ := Load(root)
	s := set.Skills()[0]
	if len(s.Unlocks) != 2 {
		t.Fatalf("unlocks=%v diag=%+v", s.Unlocks, s.Diagnostics)
	}
	for _, d := range s.Diagnostics {
		if d.Kind == DiagDecoratedKey {
			t.Fatalf("注释行不该报 DecoratedKey: %+v", s.Diagnostics)
		}
	}
}

func TestInlineCommentsAndScalarKeyEndList(t *testing.T) {
	root := t.TempDir()
	write(t, root, "i", "---\nname: i\ndescription: 带 # 的正文\nallowed-tools: xk__skill_load # 主入口\n---\nb\n")
	set, _ := Load(root)
	s := set.Skills()[0]
	if len(s.AllowedTools) != 1 || s.AllowedTools[0] != "xk__skill_load" {
		t.Fatalf("allowed=%v", s.AllowedTools)
	}
	if !strings.Contains(s.Description, "#") {
		t.Fatalf("description 的 # 不该被剥: %q", s.Description)
	}
}

func TestPayloadModes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "s", "---\nname: s\ndescription: d\n---\n正文正文\n")
	set, _ := Load(root)
	if set.Payload(ModeOff, 1000, 2000) != "" {
		t.Fatal("off 必须不注入")
	}
	idx := set.Payload(ModeIndex, 1000, 2000)
	if !strings.Contains(idx, "- s: d") {
		t.Fatalf("index=%q", idx)
	}
	full := set.Payload(ModeFull, 1000, 2000)
	if !strings.Contains(full, "正文正文") || !strings.Contains(full, "skills/full") {
		t.Fatalf("full=%q", full)
	}
}

func TestMissingDescriptionIsErrorAndExcluded(t *testing.T) {
	root := t.TempDir()
	write(t, root, "broken", "---\nname: broken\n---\n\n正文\n")
	set, _ := Load(root)
	if !set.Skills()[0].HasError() || set.ErrorCount() != 1 {
		t.Fatalf("diag=%+v", set.Skills()[0].Diagnostics)
	}
}

func TestBudgetOmitsButLabels(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a", "---\nname: a\ndescription: "+strings.Repeat("x", 200)+"\n---\nbody\n")
	write(t, root, "b", "---\nname: b\ndescription: "+strings.Repeat("y", 200)+"\n---\nbody\n")
	set, _ := Load(root)
	idx := set.Payload(ModeIndex, 120, 200)
	if !strings.Contains(idx, "omitted for budget") {
		t.Fatalf("index=%q", idx)
	}
}
