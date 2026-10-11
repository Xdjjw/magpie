package gateway

// 语义通路的响应写出与审计落盘。

import (
	"net/http"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/settings"

	"github.com/yetone/magpie/internal/gateway/xk/attach"
	"github.com/yetone/magpie/internal/gateway/xk/audit"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/gateway/xk/rewrite"
	"github.com/yetone/magpie/internal/provider"
)

// xkAttachTraceHeaders 把语义层的结论挂在响应头上,与 xkeysmith 的
// 头名保持一致(x-xkeysmith-trace / -layer / -escalated),于是同一套
// 客户端工具与审计解释两边通用。
func xkAttachTraceHeaders(w http.ResponseWriter, result *attach.LadderResult) {
	if result == nil {
		return
	}
	w.Header().Set("x-xkeysmith-trace", result.Trace.String())
	if result.Attempt != nil {
		// 采纳层(或停止时的最后一层)。
		w.Header().Set("x-xkeysmith-layer", result.Attempt.UpTo.Label())
	}
	if result.Accepted {
		w.Header().Set("x-xkeysmith-escalated", boolFlag(result.AcceptedLayer != govern.L0))
	} else {
		w.Header().Set("x-xkeysmith-escalated", "1")
		w.Header().Set("x-xkeysmith-stop", string(result.StopReason))
	}
}

func boolFlag(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// xkWriteProbe writes a non-streaming (or buffered) probe reply, with L4
// restoration applied to every string in it.
func xkWriteProbe(w http.ResponseWriter, attempt *rewrite.Attempt, res attach.ProbeResult, from provider.Protocol) (int, string) {
	if res.Stream || xkIsSSE(res) {
		return xkWriteStream(w, &attach.LadderResult{Attempt: attempt, Probe: res}, from)
	}
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

// xkWriteSynthesizedStream 把非流式探针的完整响应翻译成 SSE 下发,
// 并对文本做 L4 还原。
func xkWriteSynthesizedStream(w http.ResponseWriter, result *attach.LadderResult, from provider.Protocol) (int, string) {
	body := attach.RestoreJSON(result.Attempt, result.Probe.Body)
	if wholeEvents(from, body) == nil {
		msg := "upstream reply cannot be rendered as a protocol stream"
		return writeError(w, from, 502, msg), msg
	}
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(200)
	wholeAsStream(w, from, body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return 200, ""
}

// xkWriteStream releases the accepted native stream. L4 first decodes the
// complete reply so a placeholder split across JSON events is restored in
// the text or arguments, never across serialized event boundaries.
func xkWriteStream(w http.ResponseWriter, result *attach.LadderResult, from provider.Protocol) (int, string) {
	if result.Attempt != nil && result.Attempt.Vault != nil && !result.Attempt.Vault.IsEmpty() {
		reply, err := xkReply(from, result.Probe)
		if err != nil {
			return writeError(w, from, 502, err.Error()), err.Error()
		}
		copy := *result
		copy.Probe.Body = render(from, reply, &Request{Model: reply.Model})
		return xkWriteSynthesizedStream(w, &copy, from)
	}
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	status := result.Probe.Status
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	if _, err := w.Write(result.Probe.Body); err != nil {
		return 499, "client gone"
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return status, ""
}

// xkRecord appends one audit line per attempt, plus a masked-target table.
func (s *Server) xkRecord(settings *xkSettings, r *http.Request, p provider.Provider, model, session string, from provider.Protocol, stream bool, result *attach.LadderResult) {
	if settings.audit == nil || result == nil {
		return
	}
	wire := audit.WireOpenAi
	switch from {
	case provider.Anthropic:
		wire = audit.WireAnthropic
	case provider.Gemini:
		wire = audit.WireGemini
	}
	state, _ := r.Context().Value(xkRequestKey{}).(*xkRequestState)
	for i, rec := range result.Records {
		seq := uint32(i)
		if state != nil {
			seq = state.seq
			state.seq++
		}
		line := &audit.TraceRecord{
			TS:            time.Now().UTC(),
			Trace:         result.Trace,
			Session:       session,
			Seq:           seq,
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
			LatencyMS:     rec.LatencyMS,
			Outcome:       rec.Outcome,
			Usage:         rec.Usage,
			ToolCall:      rec.ToolCall,
		}
		if rec.StopReason != "" {
			sr := rec.StopReason
			line.StopReason = &sr
		}
		reason := rec.Reason
		line.DecisionReason = &reason
		if len(rec.RequestBody) > 0 {
			line.RequestBytes = len(rec.RequestBody)
			if settings.audit.Config.IncludeBodies {
				preview := settings.audit.Redactor.RedactTruncated(string(rec.RequestBody), settings.audit.Config.MaxBodyCaptureBytes)
				line.RequestPreview = &preview
			}
			if settings.audit.Config.RawCapture {
				raw := string(rec.RequestBody)
				line.RawRequest = &raw
			}
		}
		if len(rec.ResponseBody) > 0 {
			line.ResponseBytes = len(rec.ResponseBody)
			line.ResponseHash = audit.ShortHash(rec.ResponseBody)
			if settings.audit.Config.IncludeBodies {
				preview := settings.audit.Redactor.RedactTruncated(string(rec.ResponseBody), settings.audit.Config.MaxBodyCaptureBytes)
				line.ResponsePreview = &preview
			}
			if settings.audit.Config.RawCapture {
				raw := string(rec.ResponseBody)
				line.RawResponse = &raw
			}
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
