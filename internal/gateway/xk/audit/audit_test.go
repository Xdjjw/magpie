package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway/xk/detect"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
)

// 与 xp-audit 的 redact.rs / record.rs / sink.rs / lib.rs 测试一一对应。
// 同步纪律:Rust 侧测试变更先落那边,再同步到这里。

// ─────────────────────────── hash ───────────────────────────

func TestHashesAreStableAndDistinct(t *testing.T) {
	if ShortHash([]byte("abc")) != ShortHash([]byte("abc")) {
		t.Fatal("same input must hash equal")
	}
	if ShortHash([]byte("abc")) == ShortHash([]byte("abd")) {
		t.Fatal("distinct inputs must differ")
	}
	if len(ShortHash([]byte("abc"))) != 16 {
		t.Fatal("short hash must be 16 chars")
	}
	if len(FullHash([]byte("abc"))) != 64 {
		t.Fatal("full hash must be 64 chars")
	}
}

func TestConstantTimeEqBehavesLikeEq(t *testing.T) {
	if !ConstantTimeEq([]byte("token"), []byte("token")) ||
		ConstantTimeEq([]byte("token"), []byte("toke")) ||
		ConstantTimeEq([]byte("token"), []byte("tokeN")) {
		t.Fatal("constant-time eq mismatch")
	}
}

// ─────────────────────────── redact ───────────────────────────

func testRedactor(t *testing.T) *Redactor {
	t.Helper()
	r, err := NewRedactor(DefaultRedactPatterns())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMasksAPIKeysAndBearerTokens(t *testing.T) {
	out := testRedactor(t).Redact("Authorization: Bearer sk-abcdefghijklmnopqrstuvwxyz")
	if strings.Contains(out, "sk-abcdefghijklmnopqrstuvwxyz") || !strings.Contains(out, "redacted:") {
		t.Fatalf("out=%s", out)
	}
}

func TestMasksEmails(t *testing.T) {
	out := testRedactor(t).Redact("contact alice@example.com now")
	if strings.Contains(out, "alice@example.com") {
		t.Fatalf("out=%s", out)
	}
}

func TestSameSecretGetsSameMarker(t *testing.T) {
	r := testRedactor(t)
	a := r.Redact("key=sk-abcdefghijklmnopqrst")
	b := r.Redact("again key=sk-abcdefghijklmnopqrst")
	marker := func(s string) string {
		start := strings.Index(s, "‹")
		end := strings.Index(s, "›")
		if start < 0 || end < 0 {
			t.Fatalf("no marker in %q", s)
		}
		return s[start : end+len("›")]
	}
	if marker(a) != marker(b) {
		t.Fatalf("同一密钥必须映射到同一标记: %q vs %q", marker(a), marker(b))
	}
}

func TestRedactsNestedJSON(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(`{"headers": {"authorization": "Bearer abcdefghijklmnop"}, "n": 1}`), &v); err != nil {
		t.Fatal(err)
	}
	out := testRedactor(t).RedactValue(v)
	s := out.(map[string]any)
	headers := s["headers"].(map[string]any)
	if strings.Contains(headers["authorization"].(string), "abcdefghijklmnop") {
		t.Fatal("nested secret not redacted")
	}
	if s["n"].(float64) != 1 {
		t.Fatal("non-strings must pass through")
	}
}

func TestDisabledRedactorIsIdentity(t *testing.T) {
	r := DisabledRedactor()
	if !r.IsNoop() || r.Redact("sk-abcdefghijklmnop") != "sk-abcdefghijklmnop" {
		t.Fatal("disabled redactor must be identity")
	}
}

func TestTruncationKeepsUTF8Boundaries(t *testing.T) {
	out := DisabledRedactor().RedactTruncated("中文内容中文内容", 7)
	if !strings.HasPrefix(out, "中文") || !strings.Contains(out, "truncated") {
		t.Fatalf("out=%q", out)
	}
}

func TestBadPatternIsRejectedAtConstruction(t *testing.T) {
	if _, err := NewRedactor([]string{"("}); err == nil {
		t.Fatal("bad pattern must fail at construction")
	}
}

// ─────────────────────────── record ───────────────────────────

func TestRecordRoundTripsThroughJSON(t *testing.T) {
	preview := "…"
	rec := TraceRecord{
		TS:             nowUTC(),
		Trace:          NewTraceID(),
		Session:        "s",
		Seq:            1,
		Layer:          govern.L3,
		Attempt:        3,
		Wire:           WireAnthropic,
		Provider:       "anthropic",
		Model:          "claude",
		Stream:         true,
		Decision:       "escalate",
		DecisionReason: ptr("escalating to next rewrite layer"),
		Verdict: VerdictSummary{
			Outcome: "refused", Confidence: 0.72, Escalate: true, UpstreamOK: true,
			Families: []string{"prefix"},
			Signals:  []string{"prefix:r.cannot-assist"},
			Reason:   "refused conf=0.72",
		},
		AppliedLayers:   []govern.LayerID{govern.L1, govern.L2},
		Rules:           []string{"l2.t.crack"},
		Notes:           []string{"注入 1 个技能(index 模式)"},
		RequestHash:     "abc",
		ResponseHash:    "def",
		RequestBytes:    100,
		ResponseBytes:   50,
		RequestPreview:  &preview,
		ResponsePreview: &preview,
		LatencyMS:       42,
		Outcome:         "refused",
	}
	line, err := json.Marshal(&rec)
	if err != nil {
		t.Fatal(err)
	}
	var back TraceRecord
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatal(err)
	}
	if back.Layer != govern.L3 || back.Wire != WireAnthropic || back.Trace != rec.Trace {
		t.Fatalf("round-trip drift: %+v", back)
	}
	if !strings.Contains(rec.SummaryLine(), "L3") {
		t.Fatalf("summary=%s", rec.SummaryLine())
	}
}

// ─────────────────────────── sink / plane ───────────────────────────

func testRecord(outcome string) TraceRecord {
	return TraceRecord{
		TS: nowUTC(), Trace: NewTraceID(), Session: "s",
		Seq: 0, Layer: govern.L1, Attempt: 1, Wire: WireOpenAi,
		Provider: "openai", Model: "gpt-5",
		Decision:      "accept",
		Verdict:       VerdictSummary{Outcome: "passed", Reason: "passed"},
		AppliedLayers: []govern.LayerID{govern.L1},
		RequestHash:   "h", ResponseHash: "h",
		RequestBytes: 1, ResponseBytes: 1,
		LatencyMS: 5, Outcome: outcome,
	}
}

func TestJsonlSinkAppendsLines(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "xk-audit-test")
	_ = os.RemoveAll(dir)
	sink, err := OpenJsonl(dir, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	if err := sink.Write(ptr(testRecord("accepted"))); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(ptr(testRecord("refused"))); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(); err != nil {
		t.Fatal(err)
	}
	path, ok := sink.CurrentPath()
	if !ok {
		t.Fatal("no current path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d", len(lines))
	}
	var back TraceRecord
	if err := json.Unmarshal([]byte(lines[0]), &back); err != nil {
		t.Fatalf("line not a TraceRecord: %v (%s)", err, lines[0])
	}
	_ = os.RemoveAll(dir)
}

func TestCountingSinkClassifiesOutcomes(t *testing.T) {
	sink := &CountingSink{}
	_ = sink.Write(ptr(testRecord("accepted")))
	_ = sink.Write(ptr(testRecord("refused")))
	if sink.Written.Load() != 2 || sink.Accepted.Load() != 1 || sink.Refused.Load() != 1 {
		t.Fatalf("written=%d accepted=%d refused=%d", sink.Written.Load(), sink.Accepted.Load(), sink.Refused.Load())
	}
}

func TestTeeSinkFansOut(t *testing.T) {
	counting := &CountingSink{}
	tee := NewTee(counting)
	if err := tee.Write(ptr(testRecord("accepted"))); err != nil {
		t.Fatal(err)
	}
	if counting.Written.Load() != 1 {
		t.Fatal("tee must fan out")
	}
}

func TestNullSinkAcceptsEverything(t *testing.T) {
	_ = NullSink{}.Write(ptr(testRecord("x")))
}

func TestPlaneDisabledBuildsNullSink(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false
	p, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, isNull := p.Sink.(NullSink); !isNull {
		t.Fatalf("sink=%T", p.Sink)
	}
	_ = p.Sink.Write(ptr(testRecord("accepted")))
	if (p.Live()).RecordsWritten != 0 {
		t.Fatal("null plane must not count")
	}
}

func TestPlaneSummarizesVerdict(t *testing.T) {
	v := detect.DefaultOrPanic().JudgeText(200, "I can't help with that.")
	s := Summarize(&v)
	if s.Outcome != "refused" || !s.Escalate || len(s.Signals) == 0 {
		t.Fatalf("summary=%+v", s)
	}
	for _, fam := range s.Families {
		if fam != strings.ToLower(fam) {
			t.Fatalf("family must be lowercase: %s", fam)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func nowUTC() time.Time { return time.Now().UTC() }
