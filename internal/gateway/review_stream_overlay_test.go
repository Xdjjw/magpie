package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestReviewDeadlineEndsPartialStreamAsAnError(t *testing.T) {
	for _, hasText := range []bool{false, true} {
		name := "only-role-held"
		if hasText {
			name = "partial-text-already-sent"
		}
		t.Run(name, func(t *testing.T) {
			semanticTestEnv(t)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
				if hasText {
					io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			}))
			defer up.Close()
			if err := provider.Save(provider.Provider{ID: "review-sem", Name: "Review Semantic", Semantic: true, Key: "fixture", Models: []string{"m1"}, Chat: up.URL + "/v1"}); err != nil {
				t.Fatal(err)
			}
			if err := provider.Save(provider.Provider{ID: "review-plain", Name: "Review Plain", Key: "fixture", Models: []string{"m1"}, Chat: up.URL + "/v1", Fallback: []string{"review-sem/m1"}}); err != nil {
				t.Fatal(err)
			}
			s := New()
			s.xk().limits.WallClockMS = 250
			gateway := httptest.NewServer(lanGuard(s.Handler()))
			defer gateway.Close()
			client := gateway.Client()
			client.Timeout = 4 * time.Second
			resp, err := client.Post(gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"review-plain/m1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			recent := s.Recent()
			if len(recent) != 1 {
				t.Fatalf("recent calls=%d", len(recent))
			}
			if !strings.Contains(string(body), `"error"`) || recent[0].Error == "" {
				t.Fatalf("expired incomplete stream: HTTP=%d recent_status=%d recent_error=%q body=%q", resp.StatusCode, recent[0].Status, recent[0].Error, body)
			}
		})
	}
}
