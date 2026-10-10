package gateway

// 语义通路的响应写出与审计落盘。

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/settings"

	"github.com/yetone/magpie/internal/gateway/xk/attach"
	"github.com/yetone/magpie/internal/gateway/xk/audit"
	"github.com/yetone/magpie/internal/gateway/xk/rewrite"
	"github.com/yetone/magpie/internal/provider"
)

// xkWriteProbe writes a non-streaming (or buffered) probe reply, with L4
// restoration applied to every string in it.
func xkWriteProbe(w http.ResponseWriter, attempt *rewrite.Attempt, res attach.ProbeResult, from provider.Protocol) (int, string) {
	body := attach.RestoreJSON(attempt, res.Body)
	status := res.Status
	if status == 0 {
		status = 200
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return status, ""
}

// xkWriteStream relays an SSE reply through the incremental restorer: a
// placeholder split across chunks is never sent half-restored.
func xkWriteStream(w http.ResponseWriter, result *attach.LadderResult, from provider.Protocol) (int, string) {
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(200)
	restorer := attach.NewRestoringWriter(result.Attempt)
	// The probe already carries the whole reply (probe_then_replay): replay
	// it through the restorer rather than holding a second upstream stream.
	for _, chunk := range xkSplitSSE(result.Probe.Body) {
		if _, err := w.Write(restorer.Push(chunk)); err != nil {
			return 499, "client gone"
		}
	}
	if _, err := w.Write(restorer.Finish()); err != nil {
		return 499, "client gone"
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return 200, ""
}

// xkSplitSSE cuts an SSE body into lines, keeping the terminator.
func xkSplitSSE(body []byte) [][]byte {
	var out [][]byte
	rest := body
	for len(rest) > 0 {
		i := indexByte(rest, '\n')
		if i < 0 {
			out = append(out, rest)
			break
		}
		out = append(out, rest[:i+1])
		rest = rest[i+1:]
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// xkRecord appends one audit line per attempt, plus a masked-target table.
func (s *Server) xkRecord(settings *xkSettings, r *http.Request, p provider.Provider, model string, from provider.Protocol, stream bool, latencyMS int64, result *attach.LadderResult) {
	if settings.audit == nil || result == nil {
		return
	}
	wire := audit.WireOpenAi
	if from == provider.Anthropic {
		wire = audit.WireAnthropic
	}
	trace := audit.NewTraceID()
	session := xkSessionKey(r, mustEnvelope(result), model)
	for i, rec := range result.Records {
		line := &audit.TraceRecord{
			TS:            time.Now().UTC(),
			Trace:         trace,
			Session:       session,
			Seq:           uint32(i),
			Layer:         rec.Layer,
			Attempt:       uint8(rec.Layer),
			Wire:          wire,
			Provider:      p.Name,
			Model:         model,
			Stream:        stream,
			Decision:      rec.Decision,
			Verdict:       rec.Verdict,
			AppliedLayers: rec.Applied,
			Rules:         rec.Rules,
			Notes:         rec.Notes,
			RequestHash:   rec.RequestHash,
			MaskedTargets: rec.MaskedTargets,
			LatencyMS:     uint64(latencyMS),
			Outcome:       rec.Outcome,
		}
		if rec.StopReason != "" {
			sr := rec.StopReason
			line.StopReason = &sr
		}
		reason := rec.Reason
		line.DecisionReason = &reason
		if body, err := json.Marshal(result.Attempt.Envelope.Body); err == nil {
			line.RequestBytes = len(body)
			preview := settings.audit.Redactor.RedactTruncated(string(body), settings.audit.Config.MaxBodyCaptureBytes)
			line.RequestPreview = &preview
		}
		_ = settings.audit.Sink.Write(line)
	}
}

func mustEnvelope(result *attach.LadderResult) *rewrite.Envelope {
	if result.Attempt == nil {
		return nil
	}
	return result.Attempt.Envelope
}

// xkAuditDir places the semantic-plane audit beside magpie's own state.
func (s *Server) xkAuditDir() string {
	return filepath.Join(settings.Dir(), "xkeysmith", "audit")
}

// xkStatusLine is a one-line summary for logs.
func xkStatusLine(result *attach.LadderResult) string {
	if result == nil {
		return "xkeysmith: no result"
	}
	if result.Accepted {
		return "xkeysmith: " + result.Reason
	}
	return "xkeysmith: " + result.Reason + " (" + string(result.StopReason) + ")"
}

var _ = io.Discard
