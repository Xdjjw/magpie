package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestProviderSavePreservesAndChangesSemantic(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "semantic-test", Name: "Semantic Test", Models: []string{"m1"}, Chat: "http://127.0.0.1:59999/v1", Key: "fixture", Semantic: true}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	for _, tc := range []struct {
		field string
		want  bool
	}{{"", true}, {`,"semantic":false`, false}, {`,"semantic":true`, true}} {
		rec := httptest.NewRecorder()
		body := `{"id":"semantic-test","name":"Semantic Test","models":["m1"],"chat":"http://127.0.0.1:59999/v1"` + tc.field + `}`
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/provider/save", strings.NewReader(body)))
		got, err := provider.Find("semantic-test")
		if err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || got.Semantic != tc.want {
			t.Fatalf("field=%q status=%d semantic=%v want=%v", tc.field, rec.Code, got.Semantic, tc.want)
		}
	}
}
