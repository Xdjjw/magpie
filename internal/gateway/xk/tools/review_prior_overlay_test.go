package tools

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReviewCloseCancelsPendingMCPInitialize(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	defer close(release)
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	cfg.CallTimeoutMS = 2000
	p := BuildAsync(cfg, skillsFixture(), []McpServerCfg{{Name: "pending", Enabled: true, Transport: "http", URL: srv.URL}})
	defer p.Close()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("initialize never started")
	}
	p.Close()
	select {
	case <-canceled:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Plane.Close returned while MCP initialize remained connected")
	}
}
