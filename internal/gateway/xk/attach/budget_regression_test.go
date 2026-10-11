package attach

import (
	"testing"

	"github.com/yetone/magpie/internal/gateway/xk/govern"
)

func TestLadderChargesTokensOnce(t *testing.T) {
	cfg := ladderConfig(t)
	cfg.Limits.TokenBudget = 100
	fake := &fakeUpstream{refuseUntil: 1}
	result, err := Run(cfg, chatEnvelope(t, "hello"), func(body []byte) ProbeResult {
		r := fake.forward(body)
		r.InputTokens, r.OutputTokens = 40, 30
		return r
	}, chatObservation)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.AcceptedLayer != govern.L1 || fake.calls != 2 {
		t.Fatalf("70 tokens must leave room for the next layer: accepted=%v layer=%s calls=%d stop=%s", result.Accepted, result.AcceptedLayer.Label(), fake.calls, result.StopReason)
	}
}
