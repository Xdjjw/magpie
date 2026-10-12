package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestReviewSharedDeadlineThroughHTTP(t *testing.T) {
	for _, firstIsSemantic := range []bool{true, false} {
		name := "semantic-first"
		if !firstIsSemantic {
			name = "ordinary-first-semantic-fallback"
		}
		t.Run(name, func(t *testing.T) {
			semanticTestEnv(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
					w.WriteHeader(504)
				}
			}))
			defer up.Close()
			sem := provider.Provider{ID: "review-sem", Name: "Review Semantic", Semantic: true, Key: "fixture", Models: []string{"m1"}, Chat: up.URL + "/v1"}
			if err := provider.Save(sem); err != nil {
				t.Fatal(err)
			}
			model := "review-sem/m1"
			if !firstIsSemantic {
				native := provider.Provider{ID: "review-native", Name: "Review Native", Key: "fixture", Models: []string{"m1"}, Chat: up.URL + "/v1", Fallback: []string{"review-sem/m1"}}
				if err := provider.Save(native); err != nil {
					t.Fatal(err)
				}
				model = "review-native/m1"
			}
			s := New()
			s.xk().limits.WallClockMS = 100
			gateway := httptest.NewServer(lanGuard(s.Handler()))
			defer gateway.Close()
			client := gateway.Client()
			client.Timeout = 3 * time.Second
			resp, err := client.Post(gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			recent := s.Recent()
			if len(recent) != 1 {
				t.Fatalf("recent calls=%d", len(recent))
			}
			if resp.StatusCode != http.StatusGatewayTimeout || len(body) == 0 || recent[0].Status != http.StatusGatewayTimeout {
				t.Fatalf("shared deadline: HTTP=%d body=%q recent_status=%d recent_error=%q", resp.StatusCode, body, recent[0].Status, recent[0].Error)
			}
		})
	}
}

func TestReviewGeminiIndexRequiresAUsableLoader(t *testing.T) {
	semanticTestEnv(t)
	capSetup(t, &capFake{reply: func(int, map[string]any) map[string]any { return chatAnswer("ok") }})
	t.Setenv("XKEYSMITH_SKILLS_MODE", "index")
	var seen []map[string]any
	semanticProvider(t, provider.Gemini, func(w http.ResponseWriter, r *http.Request) {
		var doc map[string]any
		json.NewDecoder(r.Body).Decode(&doc)
		seen = append(seen, doc)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(seen) == 1 {
			io.WriteString(w, sse(`data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"SAFETY"}]}`))
		} else {
			io.WriteString(w, sse(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
		}
	})
	code, body := xkPost(t, "/v1beta/models/semantic-test/m1:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if code != 200 || len(seen) != 2 {
		t.Fatalf("HTTP=%d calls=%d body=%s", code, len(seen), body)
	}
	request, _ := json.Marshal(seen[1])
	if strings.Contains(string(request), "local-audit-lab/skills") && seen[1]["tools"] == nil {
		t.Fatal("Gemini L1 received the skills index but no callable skill loader")
	}
}
