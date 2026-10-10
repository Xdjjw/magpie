package govern

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway/xk/detect"
)

// 与 xp-govern 的 budget.rs / plan.rs / session.rs 测试一一对应。
// 同步纪律:Rust 侧测试变更先落那边,再同步到这里。

func ptrLayer(l LayerID) *LayerID { return &l }

// ─────────────────────────── budget ───────────────────────────

func TestFreshBudgetAllowsCalls(t *testing.T) {
	b := NewBudget(DefaultLimits())
	if _, ok := b.Check(); !ok {
		t.Fatal("fresh budget must allow calls")
	}
	if b.RemainingCalls() != 6 {
		t.Fatalf("remaining=%d want 6", b.RemainingCalls())
	}
}

func TestCallCeilingStopsEarly(t *testing.T) {
	cfg := DefaultLimits()
	cfg.MaxUpstreamCalls = 2
	b := NewBudget(cfg)
	b.NoteCall(10)
	if _, ok := b.Check(); !ok {
		t.Fatal("second call should fit")
	}
	b.NoteCall(10)
	if reason, ok := b.Check(); ok || reason != StopCallsExhausted {
		t.Fatalf("got ok=%v reason=%s", ok, reason)
	}
}

func TestTokenBudgetStopsEarly(t *testing.T) {
	cfg := DefaultLimits()
	cfg.TokenBudget = 100
	b := NewBudget(cfg)
	b.NoteCall(120)
	if reason, ok := b.Check(); ok || reason != StopTokenBudgetExhausted {
		t.Fatalf("got ok=%v reason=%s", ok, reason)
	}
}

func TestWallClockStopsEarly(t *testing.T) {
	cfg := DefaultLimits()
	cfg.WallClockMS = 1_000
	b := NewBudget(cfg)
	b.SimulateElapsed(1_500)
	if reason, ok := b.Check(); ok || reason != StopWallClockExhausted {
		t.Fatalf("got ok=%v reason=%s", ok, reason)
	}
	if b.RemainingMS() != 0 {
		t.Fatalf("remaining=%d want 0", b.RemainingMS())
	}
}

func TestToolRoundsDoNotConsumeTheLadderCallBudget(t *testing.T) {
	cfg := DefaultLimits()
	cfg.MaxUpstreamCalls = 1
	cfg.TokenBudget = 1_000
	b := NewBudget(cfg)
	b.NoteCall(10) // 阶梯用掉唯一一次
	if reason, _ := b.Check(); reason != StopCallsExhausted {
		t.Fatalf("reason=%s", reason)
	}
	// 但工具轮仍可继续,直到自己的上限(由 tools.max_tool_rounds 管)
	b.NoteToolRound(10)
	if b.ToolRoundsUsed() != 1 {
		t.Fatal("tool round not counted")
	}
	if _, ok := b.CheckResources(); !ok {
		t.Fatal("tool round resources must still be fine")
	}
	if reason, _ := b.Check(); reason != StopCallsExhausted {
		t.Fatal("阶梯预算不受影响")
	}
}

func TestToolRoundsStillRespectTokenBudget(t *testing.T) {
	cfg := DefaultLimits()
	cfg.TokenBudget = 100
	b := NewBudget(cfg)
	b.NoteToolRound(200)
	if reason, ok := b.CheckResources(); ok || reason != StopTokenBudgetExhausted {
		t.Fatalf("got ok=%v reason=%s", ok, reason)
	}
}

func TestSnapshotIsSerializableForAudit(t *testing.T) {
	b := NewBudget(DefaultLimits())
	data, err := json.Marshal(b.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"max_calls":6`) {
		t.Fatalf("json=%s", data)
	}
}

// ─────────────────────────── plan ───────────────────────────

func refusedVerdict() detect.Verdict {
	return detect.DefaultOrPanic().JudgeText(200, "I can't help with that.")
}

func structuredRefusalVerdict() detect.Verdict {
	return detect.DefaultOrPanic().Judge(&detect.Observation{
		Status: 200, FinishReason: "content_filter"})
}

func passedVerdict() detect.Verdict {
	return detect.DefaultOrPanic().JudgeText(200, "Here you go:\n```rust\nfn f() {}\n```")
}

func allLayers() []LayerID {
	out := make([]LayerID, len(AllLayers))
	copy(out, AllLayers)
	return out
}

func planInput(v *detect.Verdict, current LayerID, b *Budget, enabled []LayerID) *PlanInput {
	return &PlanInput{
		Verdict:                 v,
		CurrentLayer:            current,
		MaxLayer:                L4,
		EnabledLayers:           enabled,
		Budget:                  b,
		SkipLexicalOnStructured: true,
	}
}

func TestPlanAcceptsGoodResponse(t *testing.T) {
	v := passedVerdict()
	p := Plan_(planInput(&v, L0, NewBudget(DefaultLimits()), allLayers()))
	if p.Decision.Kind != DecisionAccept {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanEscalatesOnRefusal(t *testing.T) {
	v := refusedVerdict()
	p := Plan_(planInput(&v, L0, NewBudget(DefaultLimits()), allLayers()))
	if p.Decision.Label() != "escalate:L1" {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanStopsWhenLayersAreExhausted(t *testing.T) {
	v := refusedVerdict()
	p := Plan_(planInput(&v, L4, NewBudget(DefaultLimits()), allLayers()))
	if p.Decision.Kind != DecisionStop || p.Decision.Stop != StopLayersExhausted {
		t.Fatalf("got %s", p.Decision.Label())
	}
	if !p.Decision.Stop.IsExhaustion() {
		t.Fatal("layers exhaustion must count as exhaustion")
	}
}

func TestPlanStructuredEvidenceSkipsLexicalLayer(t *testing.T) {
	v := structuredRefusalVerdict()
	b := NewBudget(DefaultLimits())
	layers := allLayers()
	// L0 仍然先走 L1(上下文伪装对 policy 级拒绝是有意义的)
	p0 := Plan_(planInput(&v, L0, b, layers))
	if p0.Decision.Label() != "escalate:L1" {
		t.Fatalf("got %s", p0.Decision.Label())
	}
	if strings.Contains(p0.Reason, "skipping") {
		t.Fatalf("reason=%q", p0.Reason)
	}
	// 从 L1 出发时,词面层(L2)被跳过,直接进 L3
	p1 := Plan_(planInput(&v, L1, b, layers))
	if p1.Decision.Label() != "escalate:L3" {
		t.Fatalf("got %s", p1.Decision.Label())
	}
	if !strings.Contains(p1.Reason, "skipping lexical") {
		t.Fatalf("reason=%q", p1.Reason)
	}
}

func TestPlanLexicalLayerIsUsedForLexicalRefusals(t *testing.T) {
	v := refusedVerdict()
	p := Plan_(planInput(&v, L1, NewBudget(DefaultLimits()), allLayers()))
	if p.Decision.Label() != "escalate:L2" {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanLexicalSkipCanBeDisabled(t *testing.T) {
	v := structuredRefusalVerdict()
	i := planInput(&v, L0, NewBudget(DefaultLimits()), allLayers())
	i.SkipLexicalOnStructured = false
	if p := Plan_(i); p.Decision.Label() != "escalate:L1" {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanDisabledLayersAreSkipped(t *testing.T) {
	v := refusedVerdict()
	p := Plan_(planInput(&v, L0, NewBudget(DefaultLimits()), []LayerID{L0, L3}))
	if p.Decision.Label() != "escalate:L3" {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanStopsWhenOnlyDisabledLayersRemain(t *testing.T) {
	v := refusedVerdict()
	p := Plan_(planInput(&v, L0, NewBudget(DefaultLimits()), []LayerID{L0}))
	if p.Decision.Kind != DecisionStop || p.Decision.Stop != StopLayersExhausted {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanRespectsMaxLayer(t *testing.T) {
	v := refusedVerdict()
	b := NewBudget(DefaultLimits())
	i := planInput(&v, L0, b, allLayers())
	i.MaxLayer = L1
	if p := Plan_(i); p.Decision.Label() != "escalate:L1" {
		t.Fatalf("got %s", p.Decision.Label())
	}
	i.CurrentLayer = L1
	if p := Plan_(i); p.Decision.Kind != DecisionStop {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanCallBudgetStopsEscalation(t *testing.T) {
	v := refusedVerdict()
	cfg := DefaultLimits()
	cfg.MaxUpstreamCalls = 1
	b := NewBudget(cfg)
	b.NoteCall(1)
	p := Plan_(planInput(&v, L0, b, allLayers()))
	if p.Decision.Stop != StopCallsExhausted {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanWallClockStopsEscalation(t *testing.T) {
	v := refusedVerdict()
	cfg := DefaultLimits()
	cfg.WallClockMS = 500
	b := NewBudget(cfg)
	b.SimulateElapsed(900)
	p := Plan_(planInput(&v, L0, b, allLayers()))
	if p.Decision.Stop != StopWallClockExhausted {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanUpstreamErrorIsPassedThroughNotEscalated(t *testing.T) {
	v := detect.DefaultOrPanic().Judge(&detect.Observation{Status: 502, ErrorMessage: "bad gateway"})
	p := Plan_(planInput(&v, L0, NewBudget(DefaultLimits()), allLayers()))
	if p.Decision.Stop != StopUpstreamError {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestPlanUncertainWithoutEscalationIsAccepted(t *testing.T) {
	cfg := detect.DefaultConfig()
	cfg.UncertainEscalates = false
	cfg.Patterns = []detect.Pattern{{ID: "mid", Regex: "hedge", Scope: detect.ScopePrefix, Weight: 0.4, Enabled: true}}
	cfg.AcceptPatterns = nil
	d, err := detect.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	v := d.JudgeText(200, "hedge")
	if v.Outcome != detect.Uncertain {
		t.Fatalf("outcome=%s", v.Outcome)
	}
	p := Plan_(planInput(&v, L0, NewBudget(DefaultLimits()), allLayers()))
	if p.Decision.Kind != DecisionAccept {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

func TestDecisionLabelIsStableAndCompact(t *testing.T) {
	if (Decision{Kind: DecisionAccept}).Label() != "accept" {
		t.Fatal()
	}
	if (Decision{Kind: DecisionEscalate, Layer: L3}).Label() != "escalate:L3" {
		t.Fatal()
	}
	if (Decision{Kind: DecisionStop, Stop: StopCallsExhausted}).Label() != "stop:calls_exhausted" {
		t.Fatal()
	}
}

func TestDecisionSerializesForAudit(t *testing.T) {
	data, err := json.Marshal(Decision{Kind: DecisionEscalate, Layer: L3})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["kind"] != "escalate" || m["layer"] != "L3" {
		t.Fatalf("json=%s", data)
	}
}

func TestPlanTransportPolicy400EscalatesInsteadOfStopping(t *testing.T) {
	v := detect.DefaultOrPanic().Judge(&detect.Observation{
		Status: 400, ErrorCode: "content_policy_violation",
		ErrorMessage: "The request was rejected due to content policy"})
	if !v.Escalate || v.UpstreamOK {
		t.Fatalf("escalate=%v upstream_ok=%v", v.Escalate, v.UpstreamOK)
	}
	p := Plan_(planInput(&v, L0, NewBudget(DefaultLimits()), allLayers()))
	// 400 政策拒绝具有结构化证据,应当正常升级(L0 -> L1)
	if p.Decision.Label() != "escalate:L1" {
		t.Fatalf("got %s", p.Decision.Label())
	}
}

// ─────────────────────────── session ───────────────────────────

func TestSessionRemembersSuccessfulLayer(t *testing.T) {
	reg := NewSessionRegistry(DefaultLimits())
	cfg := DefaultPipelineConfig()
	if got := reg.EntryLayer("s1", &cfg, L0); got != L0 {
		t.Fatal(got)
	}
	reg.Record("s1", &cfg, ptrLayer(L3), true)
	if got := reg.EntryLayer("s1", &cfg, L0); got != L3 {
		t.Fatalf("got %s", got)
	}
}

func TestSessionDowngradesAfterRepeatedSuccess(t *testing.T) {
	reg := NewSessionRegistry(DefaultLimits())
	cfg := DefaultPipelineConfig()
	reg.Record("s1", &cfg, ptrLayer(L3), true)
	if got := reg.EntryLayer("s1", &cfg, L0); got != L3 {
		t.Fatalf("got %s", got)
	}
	// 第二次连续成功 → 降一层
	reg.Record("s1", &cfg, ptrLayer(L3), true)
	if got := reg.EntryLayer("s1", &cfg, L0); got != L2 {
		t.Fatalf("got %s", got)
	}
}

func TestSessionRespectsEntryFloor(t *testing.T) {
	reg := NewSessionRegistry(DefaultLimits())
	cfg := DefaultPipelineConfig()
	cfg.SessionEntryFloor = L2
	reg.Record("s1", &cfg, ptrLayer(L1), true)
	if got := reg.EntryLayer("s1", &cfg, L0); got != L2 {
		t.Fatalf("got %s", got)
	}
}

func TestSessionTotalFailureJumpsToTopLayer(t *testing.T) {
	reg := NewSessionRegistry(DefaultLimits())
	cfg := DefaultPipelineConfig()
	reg.Record("s1", &cfg, nil, true)
	if got := reg.EntryLayer("s1", &cfg, L0); got != L4 {
		t.Fatalf("got %s", got)
	}
}

func TestSessionMemoryCanBeDisabled(t *testing.T) {
	reg := NewSessionRegistry(DefaultLimits())
	cfg := DefaultPipelineConfig()
	cfg.SessionMemory = false
	reg.Record("s1", &cfg, ptrLayer(L3), true)
	if got := reg.EntryLayer("s1", &cfg, L0); got != L0 {
		t.Fatalf("got %s", got)
	}
	if reg.Len() != 0 {
		t.Fatalf("len=%d", reg.Len())
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	reg := NewSessionRegistry(DefaultLimits())
	cfg := DefaultPipelineConfig()
	reg.Record("a", &cfg, ptrLayer(L4), true)
	if got := reg.EntryLayer("b", &cfg, L0); got != L0 {
		t.Fatalf("b got %s", got)
	}
	if got := reg.EntryLayer("a", &cfg, L0); got != L4 {
		t.Fatalf("a got %s", got)
	}
}

func TestExpiredSessionsAreSwept(t *testing.T) {
	limits := DefaultLimits()
	limits.SessionTTLSeconds = 0
	reg := NewSessionRegistry(limits)
	cfg := DefaultPipelineConfig()
	reg.Record("a", &cfg, ptrLayer(L2), true)
	if reg.Len() != 1 {
		t.Fatalf("len=%d", reg.Len())
	}
	reg.sweepIfNeeded()
	if reg.Len() != 0 {
		t.Fatalf("len=%d", reg.Len())
	}
}

func TestSessionCapacityIsBounded(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSessions = 4
	limits.SessionTTLSeconds = 3600
	reg := NewSessionRegistry(limits)
	cfg := DefaultPipelineConfig()
	for i := 0; i < 40; i++ {
		reg.Record("s"+strings.Repeat("x", i)+"-"+strings.Repeat("y", i), &cfg, ptrLayer(L1), false)
		reg.sweepIfNeeded()
	}
	reg.sweepIfNeeded()
	if reg.Len() > 4 {
		t.Fatalf("容量上限必须被尊重,实际 %d", reg.Len())
	}
}

func TestUnlockSetRoundTripsPerSession(t *testing.T) {
	reg := NewSessionRegistry(DefaultLimits())
	cfg := DefaultPipelineConfig()
	if s, p := reg.UnlockSet("a"); len(s) != 0 || len(p) != 0 {
		t.Fatal("expected empty")
	}
	reg.RecordUnlocks("a", &cfg, []string{"api-recon"}, []string{"xk__mcp__fs__*"})
	skills, patterns := reg.UnlockSet("a")
	if len(skills) != 1 || skills[0] != "api-recon" || len(patterns) != 1 || patterns[0] != "xk__mcp__fs__*" {
		t.Fatalf("%v %v", skills, patterns)
	}
	if s, _ := reg.UnlockSet("b"); len(s) != 0 {
		t.Fatal("会话之间必须隔离")
	}
	// 重复写不产生重复项
	reg.RecordUnlocks("a", &cfg, []string{"api-recon"}, []string{"xk__mcp__fs__*"})
	if s, _ := reg.UnlockSet("a"); len(s) != 1 {
		t.Fatalf("len=%d", len(s))
	}
}

func TestSessionSnapshotIsSerializable(t *testing.T) {
	reg := NewSessionRegistry(DefaultLimits())
	cfg := DefaultPipelineConfig()
	reg.Record("a", &cfg, ptrLayer(L1), false)
	data, err := json.Marshal(reg.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"key":"a"`) {
		t.Fatalf("json=%s", data)
	}
}
