package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/gateway/xk/attach"
	"github.com/yetone/magpie/internal/gateway/xk/audit"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/provider"
)

type xkRequestKey struct{}

// One client request shares resource limits and trace identity across provider
// candidates, rewrite layers and gateway tool rounds.
type xkRequestState struct {
	budget   *govern.Budget
	trace    audit.TraceID
	seq      uint32
	acted    atomic.Bool
	terminal atomic.Bool
}

func xkRequest(r *http.Request, limits govern.Limits) (*http.Request, *xkRequestState, context.CancelFunc) {
	if state, ok := r.Context().Value(xkRequestKey{}).(*xkRequestState); ok {
		return r, state, func() {}
	}
	state := &xkRequestState{budget: govern.NewBudget(limits), trace: audit.NewTraceID()}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(limits.WallClockMS)*time.Millisecond)
	ctx = context.WithValue(ctx, xkRequestKey{}, state)
	return r.WithContext(ctx), state, cancel
}

func xkActed(ctx context.Context) bool {
	state, _ := ctx.Value(xkRequestKey{}).(*xkRequestState)
	return state != nil && state.acted.Load()
}

// xkTerminal says the semantic layer has decided this request is over: its
// ladder is out or its budget is spent, so no other candidate of it can
// answer and the reply already written is the one to send. An upstream
// error is not one of these: that one is what failover is for.
func xkTerminal(ctx context.Context) bool {
	state, _ := ctx.Value(xkRequestKey{}).(*xkRequestState)
	return state != nil && state.terminal.Load()
}

// xkProbeWriter holds a native reply until the ladder accepts it. It implements
// Flush without committing to the client; oversized replies cancel the attempt
// rather than silently judging a truncated JSON body.
type xkProbeWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	ctx    context.Context
	cancel context.CancelFunc
	err    error
}

func (w *xkProbeWriter) Header() http.Header { return w.header }
func (w *xkProbeWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *xkProbeWriter) Flush() {}
func (w *xkProbeWriter) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return 0, err
	}
	if w.body.Len()+len(b) > 8<<20 {
		w.err = errors.New("semantic response exceeds 8 MiB")
		w.cancel()
		return 0, w.err
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

func (s *Server) xkNativeProbe(r *http.Request, from provider.Protocol, p provider.Provider, model string, body []byte, call *Call) (attach.ProbeResult, http.Header) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	buffer := &xkProbeWriter{header: http.Header{}, ctx: ctx, cancel: cancel}
	hw := newHoldWriter(buffer, false)
	hw.ctx = ctx
	status, _ := s.attemptNative(hw, r.WithContext(ctx), from, p, model, body, call)
	probe := attach.ProbeResult{Status: status, Body: buffer.body.Bytes(), Err: buffer.err,
		InputTokens: uint64(max(call.Usage.prompt(), 0)), OutputTokens: uint64(max(call.Usage.Output, 0)),
		Stream: strings.HasPrefix(buffer.header.Get("Content-Type"), "text/event-stream")}
	if status == 0 {
		probe.Status = buffer.status
	}
	return probe, buffer.header
}

// Read replies using the same event decoders as translation. Whole JSON
// replies first become the native protocol's complete event sequence.
func xkReply(proto provider.Protocol, probe attach.ProbeResult) (Result, error) {
	var col collector
	decode := decoder(proto)
	if probe.Stream || xkIsSSE(probe) {
		err := readSSE(bytes.NewReader(probe.Body), func(_, data string) error { return decode(data, col.add) })
		if err != nil {
			return Result{}, err
		}
	} else {
		events := wholeEvents(proto, probe.Body)
		if events == nil {
			if proto == provider.Gemini {
				if err := decode(string(probe.Body), col.add); err != nil {
					return Result{}, err
				}
			} else {
				return Result{}, errors.New("upstream did not return a protocol reply")
			}
		}
		for _, event := range events {
			data, ok := event.data.(string)
			if !ok {
				raw, err := json.Marshal(event.data)
				if err != nil {
					return Result{}, err
				}
				data = string(raw)
			}
			if err := decode(data, col.add); err != nil {
				return Result{}, err
			}
		}
	}
	result := col.finish()
	if col.err != "" {
		return result, errors.New(col.err)
	}
	return result, nil
}

func xkSumUsage(total *Usage, next Usage) {
	input, output, read, write, write1h, reasoning := total.Input, total.Output, total.CacheRead, total.CacheWrite, total.CacheWrite1h, total.Reasoning
	total.add(next)
	total.Input = input + next.Input
	total.Output = output + next.Output
	total.CacheRead = read + next.CacheRead
	total.CacheWrite = write + next.CacheWrite
	total.CacheWrite1h = write1h + next.CacheWrite1h
	total.Reasoning = reasoning + next.Reasoning
}
