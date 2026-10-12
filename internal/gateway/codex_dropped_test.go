package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// edgeDropped is what ylorn's ChatGPT backend answered to a request whose
// screenshot took it past about 1 MiB (Discord, v0.1.1179), and
// ScarfQian's to a 1.1 MiB computer-use turn (#1628): Envoy's own 503
// behind Cloudflare, as text, before any reply.
const edgeDropped = "upstream connect error or disconnect/reset before headers. reset reason: remote connection failure"

// dropsBig stands in for that ChatGPT backend: a body past 1 MiB is
// answered with edgeDropped the first *drops times (every time while it
// is below zero) and served after; a smaller one is served. It notes the
// account asked each time.
func dropsBig(t *testing.T, tried *[]string, drops *int) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*tried = append(*tried, r.Header.Get("chatgpt-account-id"))
		if len(b) > 1<<20 && *drops != 0 {
			if *drops > 0 {
				*drops--
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Server", "cloudflare")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, edgeDropped)
			return
		}
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"r1","model":"gpt-5.5"}}`,
			`data: {"type":"response.output_text.delta","delta":"pong"}`,
			`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":7,"output_tokens":1}}}`))
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
}

// screenshotTurn is a Responses turn with a PNG screenshot of about n
// bytes inline, base64 as an agent sends it. Its bytes aren't an image
// any decoder reads, so nothing on the way makes it smaller.
func screenshotTurn(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n*3/4)
	rand.Read(raw)
	b, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "stream": true, "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "what does this screenshot show?"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)},
		}}}})
	return string(b)
}

// ScarfQian (#1628): with two ChatGPT accounts on, a 1.2 MiB turn the
// backend closes on twice, then answers, is answered: asked of acct-1,
// then acct-2, then acct-2 again, and nobody rests, since no account is at
// fault for it. On a2b96139c's code the agent got a 413 after one try,
// which Codex never asks again.
func TestCodexResetBigBodyAskedAgain(t *testing.T) {
	codexSignedIn(t, "spare@example.com")
	var tried []string
	drops := 2
	dropsBig(t, &tried, &drops)

	code, body := codexPost(t, screenshotTurn(t, 1200<<10))
	if code != 200 || !strings.Contains(body, "pong") {
		t.Fatalf("status %d, want 200 pong: %s", code, body)
	}
	if strings.Join(tried, ",") != "acct-1,acct-2,acct-2" {
		t.Errorf("asked %v, want acct-1,acct-2,acct-2", tried)
	}
	if r := restingNow(t); len(r) != 0 {
		t.Errorf("rest over a reset of a large request: %v", r)
	}
}

// Closed on every time, the turn is asked of each account and the last
// again, as for a 503, and the agent is told the 503 — which Codex asks
// again for — with what may help should it keep happening. Nobody rests,
// and nothing is remembered of it: the next turn, as large, goes to acct-1
// and is answered once the backend takes it.
func TestCodexResetBigBodyStays503(t *testing.T) {
	codexSignedIn(t, "spare@example.com")
	var tried []string
	drops := -1
	dropsBig(t, &tried, &drops)

	code, body := codexPost(t, screenshotTurn(t, 1200<<10))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", code, body)
	}
	for _, want := range []string{"MiB", "compact", "image", "reset reason"} {
		if !strings.Contains(body, want) {
			t.Errorf("the agent isn't told %q: %s", want, body)
		}
	}
	if strings.Join(tried, ",") != "acct-1,acct-2,acct-2,acct-2" {
		t.Errorf("asked %v, want acct-1 then acct-2 three times", tried)
	}
	if r := restingNow(t); len(r) != 0 {
		t.Errorf("rest over a reset of a large request: %v", r)
	}

	tried, drops = nil, 0
	code, body = codexPost(t, screenshotTurn(t, 1200<<10))
	if code != 200 || !strings.Contains(body, "pong") || strings.Join(tried, ",") != "acct-1" {
		t.Fatalf("large turn after: %d %s tried %v", code, body, tried)
	}
}

// With Codex's own sign-in alone, relayed as it came: a large request
// closed on once is asked again and answered; closed on every time, Codex
// is answered the 503 after three tries, with what may help.
func TestCodexResetBigBodyRelayed(t *testing.T) {
	codexSignedIn(t)
	var tried []string
	drops := 1
	dropsBig(t, &tried, &drops)

	code, body := codexPost(t, screenshotTurn(t, 1200<<10))
	if code != 200 || !strings.Contains(body, "pong") || len(tried) != 2 {
		t.Fatalf("closed on once: %d %s tried %v", code, body, tried)
	}

	tried, drops = nil, -1
	code, body = codexPost(t, screenshotTurn(t, 1200<<10))
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "compact") || !strings.Contains(body, "reset reason") {
		t.Fatalf("status %d: %s", code, body)
	}
	if len(tried) != 3 {
		t.Errorf("asked %d times, want 3", len(tried))
	}
}

// The same 503 to a small request is the backend's trouble, as it is in
// an outage: it is relayed as the 503 it is, for the client to try again.
func TestCodexEdgeResetSmallBodyStays503(t *testing.T) {
	codexSignedIn(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, edgeDropped)
	}))
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })

	code, body := codexPost(t, `{"model":"gpt-5.5","stream":true,"input":"ping"}`)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "reset reason") {
		t.Fatalf("status %d: %s", code, body)
	}
}
