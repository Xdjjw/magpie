package attach

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway/xk/detect"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/gateway/xk/rewrite"
)

// 阶段③ 的验收:整条阶梯用**假上游**穷举——与 Rust 版 e2e(假上游 + 真代理)
// 同一个思路,但不需要 HTTP,因为上游调用是注入的 Forward 函数。

// fakeUpstream 按「第几次调用」决定回什么:前 refuseUntil 次回拒绝。
type fakeUpstream struct {
	calls       int
	refuseUntil int
	// 记录每次收到的请求体,供断言「层是累积的」。
	seen []string
}

func (f *fakeUpstream) forward(body []byte) ProbeResult {
	f.calls++
	f.seen = append(f.seen, string(body))
	if f.calls <= f.refuseUntil {
		return ProbeResult{Status: 200, Body: []byte(
			`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"I can't help with that."}}]}`)}
	}
	return ProbeResult{Status: 200, Body: []byte(
		`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Here you go:\n` + "```" + `\nok\n` + "```" + `"}}]}`)}
}

// chatObservation 是 OpenAI Chat 方言的观察翻译(与网关里的适配同构)。
func chatObservation(_ string, res ProbeResult) *detect.Observation {
	obs := &detect.Observation{Status: res.Status}
	if res.Err != nil {
		obs.ErrorMessage = res.Err.Error()
	}
	var doc struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				Refusal   string `json:"refusal"`
				ToolCalls []any  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body, &doc); err == nil {
		if len(doc.Choices) > 0 {
			c := doc.Choices[0]
			obs.FinishReason = c.FinishReason
			obs.RefusalField = c.Message.Refusal
			obs.Text = c.Message.Content
			obs.ToolCalls = len(c.Message.ToolCalls)
		}
		if doc.Error != nil {
			obs.ErrorCode = doc.Error.Code
			obs.ErrorMessage = doc.Error.Message
		}
	}
	return obs
}

func newPipeline(t *testing.T) *rewrite.Pipeline {
	t.Helper()
	p, err := rewrite.NewPipeline(rewrite.DefaultRewriteConfig(), govern.AllLayers)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newDetector(t *testing.T) *detect.Detector {
	t.Helper()
	return detect.DefaultOrPanic()
}

func chatEnvelope(t *testing.T, content string) *rewrite.Envelope {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":`+mustJSONString(content)+`}]}`), &body); err != nil {
		t.Fatal(err)
	}
	return rewrite.NewEnvelope(body, rewrite.WireOpenAi)
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func ladderConfig(t *testing.T) *LadderConfig {
	t.Helper()
	limits := govern.DefaultLimits()
	return &LadderConfig{
		Pipeline:    newPipeline(t),
		Detector:    newDetector(t),
		Limits:      limits,
		PipelineCfg: govern.DefaultPipelineConfig(),
		EntryLayer:  govern.L0,
		MaxLayer:    govern.L4,
		Proto:       "chat",
	}
}

func TestLadderEscalatesUntilAccepted(t *testing.T) {
	fake := &fakeUpstream{refuseUntil: 2} // L0、L1 被拒,L2 通过
	cfg := ladderConfig(t)
	res, err := Run(cfg, chatEnvelope(t, "explain the crack tool"), fake.forward, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.AcceptedLayer != govern.L2 {
		t.Fatalf("accepted=%v layer=%s records=%d", res.Accepted, res.AcceptedLayer.Label(), len(res.Records))
	}
	if fake.calls != 3 {
		t.Fatalf("calls=%d want 3", fake.calls)
	}
	// 每条 attempt 一条记录,且逐层升级
	if len(res.Records) != 3 {
		t.Fatalf("records=%d", len(res.Records))
	}
	for i, want := range []govern.LayerID{govern.L0, govern.L1, govern.L2} {
		if res.Records[i].Layer != want {
			t.Fatalf("record[%d].layer=%s want %s", i, res.Records[i].Layer.Label(), want.Label())
		}
	}
	// 层是累积的:L2 的请求里仍有 L1 注入的实验室指令
	if !strings.Contains(fake.seen[2], "local-audit-lab") {
		t.Fatal("层必须累积:L2 请求里应保留 L1 的注入")
	}
}

func TestLadderStopsWhenLayersExhausted(t *testing.T) {
	fake := &fakeUpstream{refuseUntil: 99}
	res, err := Run(ladderConfig(t), chatEnvelope(t, "do the thing"), fake.forward, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted {
		t.Fatal("全层拒绝时不得采纳")
	}
	if res.StopReason != govern.StopLayersExhausted {
		t.Fatalf("stop=%s", res.StopReason)
	}
	if !res.StopReason.IsExhaustion() {
		t.Fatal("层耗尽属于正常耗尽")
	}
}

func TestLadderStopsOnCallBudget(t *testing.T) {
	fake := &fakeUpstream{refuseUntil: 99}
	cfg := ladderConfig(t)
	cfg.Limits.MaxUpstreamCalls = 2
	res, err := Run(cfg, chatEnvelope(t, "do the thing"), fake.forward, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != govern.StopCallsExhausted {
		t.Fatalf("stop=%s", res.StopReason)
	}
	if fake.calls != 2 {
		t.Fatalf("calls=%d want 2(预算封顶)", fake.calls)
	}
}

func TestLadderDoesNotEscalateUpstreamError(t *testing.T) {
	// 上游故障不是拒绝:升级毫无意义,应当立即停止并透传。
	calls := 0
	forward := func(body []byte) ProbeResult {
		calls++
		return ProbeResult{Status: 502, Body: []byte(`{"error":{"message":"bad gateway"}}`)}
	}
	res, err := Run(ladderConfig(t), chatEnvelope(t, "hello"), forward, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != govern.StopUpstreamError {
		t.Fatalf("stop=%s", res.StopReason)
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
}

func TestLadderDeadlineDuringForwardStopsAsTimeout(t *testing.T) {
	cfg := ladderConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	cfg.Context = ctx
	defer cancel()
	calls := 0
	forward := func([]byte) ProbeResult {
		calls++
		<-cfg.Context.Done()
		return ProbeResult{Err: cfg.Context.Err()}
	}
	res, err := Run(cfg, chatEnvelope(t, "hello"), forward, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != govern.StopWallClockExhausted || calls != 1 || len(res.Records) != 1 || res.Records[0].StopReason != string(govern.StopWallClockExhausted) {
		t.Fatalf("result=%+v calls=%d", res, calls)
	}
}

func TestLadderTransportPolicy400Escalates(t *testing.T) {
	// 400 + 策略码是**结构化拒绝**,必须升级而不是当故障透传。
	calls := 0
	forward := func(body []byte) ProbeResult {
		calls++
		if calls == 1 {
			return ProbeResult{Status: 400, Body: []byte(
				`{"error":{"code":"content_policy_violation","message":"rejected by safety system"}}`)}
		}
		return ProbeResult{Status: 200, Body: []byte(
			`{"choices":[{"finish_reason":"stop","message":{"content":"ok ` + "```" + `x` + "```" + `"}}]}`)}
	}
	res, err := Run(ladderConfig(t), chatEnvelope(t, "hello"), forward, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.AcceptedLayer != govern.L1 {
		t.Fatalf("accepted=%v layer=%s", res.Accepted, res.AcceptedLayer.Label())
	}
}

func TestLadderL4MasksThenRestoresResponse(t *testing.T) {
	// L4 抽象化后,模型「看到」的是占位符;响应里的占位符必须还原回真值。
	var sawPlaceholder bool
	forward := func(body []byte) ProbeResult {
		if strings.Contains(string(body), "10.20.30.40") {
			// 未被抽象化:L4 之前应是这样,这里直接拒绝以便继续升级
			return ProbeResult{Status: 200, Body: []byte(
				`{"choices":[{"finish_reason":"stop","message":{"content":"I can't help with that."}}]}`)}
		}
		if i := strings.Index(string(body), "«TGT-"); i >= 0 {
			sawPlaceholder = true
			// 模型把抽象化的目标原样回给客户端
			end := strings.Index(string(body)[i:], "»")
			ph := string(body)[i : i+end+len("»")]
			return ProbeResult{Status: 200, Body: []byte(
				`{"choices":[{"finish_reason":"stop","message":{"content":"connecting to ` + ph + ` now"}}]}`)}
		}
		return ProbeResult{Status: 200, Body: []byte(
			`{"choices":[{"finish_reason":"stop","message":{"content":"no target"}}]}`)}
	}
	cfg := ladderConfig(t)
	res, err := Run(cfg, chatEnvelope(t, "connect to 10.20.30.40"), forward, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.AcceptedLayer != govern.L4 {
		t.Fatalf("accepted=%v layer=%s stop=%s", res.Accepted, res.AcceptedLayer.Label(), res.StopReason)
	}
	if !sawPlaceholder {
		t.Fatal("L4 必须让上游看到占位符而不是真值")
	}
	restored := string(RestoreJSON(res.Attempt, res.Probe.Body))
	if !strings.Contains(restored, "10.20.30.40") || strings.Contains(restored, "«TGT-") {
		t.Fatalf("响应还原不完整: %s", restored)
	}
	// 审计里必须留下还原表
	found := false
	for _, rec := range res.Records {
		for _, mt := range rec.MaskedTargets {
			if mt.Original == "10.20.30.40" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("审计必须记录 masked_targets(否则事后无法回答改了哪些目标值)")
	}
}

func TestLadderSessionMemoryStartsAtRememberedLayer(t *testing.T) {
	sessions := govern.NewSessionRegistry(govern.DefaultLimits())
	cfg := ladderConfig(t)
	cfg.Sessions = sessions
	cfg.SessionKey = "s1"

	// 第一轮:L0 被拒 → L1 通过,记住 L1
	fake := &fakeUpstream{refuseUntil: 1}
	if _, err := Run(cfg, chatEnvelope(t, "first"), fake.forward, chatObservation); err != nil {
		t.Fatal(err)
	}
	if got := sessions.EntryLayer("s1", &cfg.PipelineCfg, govern.L0); got != govern.L1 {
		t.Fatalf("session entry=%s want L1", got.Label())
	}

	// 第二轮:直接从 L1 起步(不再白爬 L0)
	fake2 := &fakeUpstream{refuseUntil: 0}
	res, err := Run(cfg, chatEnvelope(t, "second"), fake2.forward, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Layer != govern.L1 {
		t.Fatalf("第二轮应从记忆层起步,实际 %s", res.Records[0].Layer.Label())
	}
}

// ─────────────────────────── 流式还原 ───────────────────────────

func TestRestoringWriterHoldsSplitPlaceholders(t *testing.T) {
	attempt, placeholder := maskAttempt(t)
	w := NewRestoringWriter(attempt)

	// 把占位符切成三块,分别放进三个 SSE data 行
	third := len(placeholder) / 3
	chunks := []string{
		"data: {\"delta\":\"connecting to " + placeholder[:third],
		placeholder[third : 2*third],
		placeholder[2*third:] + "\"}\n",
		"data: [DONE]\n",
	}
	var out strings.Builder
	for _, c := range chunks {
		out.Write(w.Push([]byte(c)))
	}
	out.Write(w.Finish())

	got := out.String()
	if strings.Contains(got, "«TGT") {
		t.Fatalf("绝不能下发半个占位符: %q", got)
	}
	if !strings.Contains(got, "10.20.30.40") {
		t.Fatalf("流式还原不完整: %q", got)
	}
	if !strings.Contains(got, "[DONE]") {
		t.Fatalf("终止行必须保留: %q", got)
	}
}

func TestRestoringWriterPassesThroughWhenNoMasking(t *testing.T) {
	p, _ := rewrite.NewPipeline(rewrite.DefaultRewriteConfig(), []govern.LayerID{govern.L0})
	attempt, err := p.Attempt(chatEnvelope(t, "hello"), govern.L0)
	if err != nil {
		t.Fatal(err)
	}
	w := NewRestoringWriter(attempt)
	chunk := []byte("data: {\"delta\":\"plain\"}\n")
	if got := string(w.Push(chunk)); got != string(chunk) {
		t.Fatalf("无占位符时必须原样透传: %q", got)
	}
}

func maskAttempt(t *testing.T) (*rewrite.Attempt, string) {
	t.Helper()
	p, err := rewrite.NewPipeline(rewrite.DefaultRewriteConfig(), []govern.LayerID{govern.L0, govern.L4})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := p.Attempt(chatEnvelope(t, "connect to 10.20.30.40"), govern.L4)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Vault.Len() != 1 {
		t.Fatalf("vault=%d", attempt.Vault.Len())
	}
	return attempt, attempt.Vault.Entries()[0].Placeholder
}
