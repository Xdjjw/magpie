package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway/xk/attach"
	"github.com/yetone/magpie/internal/gateway/xk/audit"
	"github.com/yetone/magpie/internal/gateway/xk/govern"
	"github.com/yetone/magpie/internal/provider"
)

type xkAuditCaptureSink struct {
	records []*audit.TraceRecord
}

func (s *xkAuditCaptureSink) Write(record *audit.TraceRecord) error {
	copy := *record
	s.records = append(s.records, &copy)
	return nil
}

func (s *xkAuditCaptureSink) Flush() error                { return nil }
func (s *xkAuditCaptureSink) CurrentPath() (string, bool) { return "", false }

func TestXkeysmithAuditCaptureOptions(t *testing.T) {
	request := []byte(`Authorization: Bearer sk-abcdefghijklmnop`)
	response := []byte(`{"choices":[{"message":{"content":"hello"}}]}`)

	for _, tc := range []struct {
		name          string
		includeBodies bool
		rawCapture    bool
		wantPreview   bool
		wantRaw       bool
	}{
		{name: "preview", includeBodies: true, wantPreview: true},
		{name: "metadata only", includeBodies: false},
		{name: "raw capture", rawCapture: true, wantRaw: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := audit.DefaultConfig()
			cfg.Enabled = false
			cfg.IncludeBodies = tc.includeBodies
			cfg.RawCapture = tc.rawCapture
			plane, err := audit.Build(cfg)
			if err != nil {
				t.Fatal(err)
			}
			capture := &xkAuditCaptureSink{}
			plane.Sink = capture
			settings := &xkSettings{audit: plane}
			requestHTTP := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			result := &attach.LadderResult{
				Trace: audit.NewTraceID(),
				Records: []attach.AttemptRecord{{
					Layer:        govern.L0,
					Decision:     "accept",
					Outcome:      "accepted",
					RequestBody:  request,
					ResponseBody: response,
				}},
			}

			New().xkRecord(settings, requestHTTP, provider.Provider{ID: "fixture", Name: "Fixture"}, "m1", "session", provider.Chat, false, result)
			if len(capture.records) != 1 {
				t.Fatalf("captured records=%d", len(capture.records))
			}
			record := capture.records[0]
			if (record.RequestPreview != nil || record.ResponsePreview != nil) != tc.wantPreview {
				t.Fatalf("preview presence request=%v response=%v want=%v", record.RequestPreview != nil, record.ResponsePreview != nil, tc.wantPreview)
			}
			if (record.RawRequest != nil || record.RawResponse != nil) != tc.wantRaw {
				t.Fatalf("raw presence request=%v response=%v want=%v", record.RawRequest != nil, record.RawResponse != nil, tc.wantRaw)
			}
			if tc.wantPreview {
				if strings.Contains(*record.RequestPreview, "sk-abcdefghijklmnop") {
					t.Fatalf("preview leaked credential: %q", *record.RequestPreview)
				}
			}
			if tc.wantRaw {
				if *record.RawRequest != string(request) || *record.RawResponse != string(response) {
					t.Fatalf("raw capture changed payload: request=%q response=%q", *record.RawRequest, *record.RawResponse)
				}
			}
		})
	}
}
