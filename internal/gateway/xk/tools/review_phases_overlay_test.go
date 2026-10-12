package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestReviewCloseCancelsEveryMCPDiscoveryPhase(t *testing.T) {
	for _, phase := range []string{"initialize", "notifications/initialized", "tools/list"} {
		t.Run(phase, func(t *testing.T) {
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var startOnce, cancelOnce sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string `json:"method"`
					ID     any    `json:"id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				if req.Method == phase {
					startOnce.Do(func() { close(started) })
					select {
					case <-r.Context().Done():
						cancelOnce.Do(func() { close(canceled) })
						return
					case <-release:
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
				}
				if req.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{}}})
			}))
			defer srv.Close()
			defer close(release)
			cfg := DefaultToolsConfig()
			cfg.Enabled = true
			cfg.CallTimeoutMS = 3000
			p := BuildAsync(cfg, skillsFixture(), []McpServerCfg{{Name: "review", Enabled: true, Transport: "http", URL: srv.URL}})
			defer p.Close()
			select {
			case <-started:
			case <-time.After(4 * time.Second):
				t.Fatal("discovery did not reach " + phase)
			}
			p.Close()
			select {
			case <-canceled:
			case <-time.After(400 * time.Millisecond):
				t.Fatal("Close left the " + phase + " request running")
			}
		})
	}
}
