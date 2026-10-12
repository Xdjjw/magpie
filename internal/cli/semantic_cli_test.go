package cli

import "github.com/yetone/magpie/internal/provider"

import "testing"

// semantic=yes|no 是 provider 的本地语义管线开关(provider.Semantic):
// 打开后该 provider 的请求走 L0-L4 改写阶梯、拒绝判定与审计。
func TestApplyPairsParsesSemantic(t *testing.T) {
	p := provider.Provider{}
	if err := applyPairs(&p, []string{"semantic=yes"}); err != nil {
		t.Fatal(err)
	}
	if !p.Semantic {
		t.Fatal("semantic=yes must set the flag")
	}
	if err := applyPairs(&p, []string{"semantic=no"}); err != nil {
		t.Fatal(err)
	}
	if p.Semantic {
		t.Fatal("semantic=no must clear the flag")
	}
	if err := applyPairs(&p, []string{"semantic=maybe"}); err == nil {
		t.Fatal("bad value must be rejected")
	}
}
