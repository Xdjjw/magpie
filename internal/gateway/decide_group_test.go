package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// decideFleet is System One APIs whose every key answers as status says
// (200 when unset), and the keys asked in turn.
type decideFleet struct {
	mu     sync.Mutex
	status map[string]int
	asked  []string
}

func (f *decideFleet) set(key string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[key] = code
}

func (f *decideFleet) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.asked
	f.asked = nil
	return out
}

func (f *decideFleet) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var q struct{ Model string }
		json.NewDecoder(r.Body).Decode(&q)
		f.mu.Lock()
		f.asked = append(f.asked, key)
		code := f.status[key]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if code != 0 && code != 200 {
			w.WriteHeader(code)
			io.WriteString(w, `{"detail":{"message":"no capacity for `+key+`"}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"model": q.Model, "answers": map[string]any{
			"levels": map[string]any{"type": "noul", "noul": 0},
			"intent": map[string]any{"type": "choice", "choice": "bug", "confidence": 0.9}},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 2}})
	}))
}

// A routing group of decision models answers /v1/systemone (ARNO on
// Discord): its keys and accounts asked in the group's order, one that
// fails as a chat's would resting and the next asked, every try in the
// routing log; asked by "group/<id>" or by the group's name.
func TestSystemOneDecisionGroupFailsOver(t *testing.T) {
	fresh(t)
	f := &decideFleet{status: map[string]int{}}
	a, b := f.serve(), f.serve()
	defer a.Close()
	defer b.Close()
	for _, p := range []provider.Provider{
		// a decision provider alone, two keys: each one a try of its own
		{ID: "ja", Name: "JA", Key: "ka1", Keys: []provider.KeyAccount{{Key: "ka2"}}, Decide: a.URL + "/v1"},
		{ID: "jb", Name: "JB", Key: "kb", Decide: b.URL + "/v1"},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.SaveGroup(provider.Group{ID: "jevs", Name: "Jevs", Members: []string{"ja/jev-latest", "jb/jev-latest"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	call := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer magpie")
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	const ask = `{"model":%q,"state":{"message":"hi"},"questions":{"intent":{"type":"choice"}}}`
	f.set("ka1", 429)
	f.set("ka2", 503)
	rec := call("/v1/systemone", strings.Replace(ask, "%q", `"group/jevs"`, 1))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"choice":"bug"`) {
		t.Fatalf("group: %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(f.took(), ","); got != "ka1,ka2,kb" {
		t.Fatalf("asked %s, want ka1,ka2,kb", got)
	}
	if n := len(s.trace.routes); n != 1 {
		t.Fatalf("traced %d", n)
	}
	r := s.trace.routes[0]
	if r.Model != "group/jevs" || r.Group == nil || r.Provider != "jb" || !r.Done || r.Status != 200 || len(r.Tries) != 3 {
		t.Fatalf("route %+v", r)
	}
	for i, want := range []int{429, 503, 200} {
		try := r.Tries[i]
		if try.Status != want || try.Model != "jev-latest" || (want != 200) != (try.Rest != nil) {
			t.Errorf("try %d: %+v, want %d, a rest when it failed", i, try, want)
		}
	}
	if len(r.Usage) != 1 || r.Usage[0].Provider != "jb" {
		t.Fatalf("usage %+v", r.Usage)
	}
	// the two that failed rest: by the group's name, the one that answered
	// goes first
	rec = call("/v1/systemone", strings.Replace(ask, "%q", `"Jevs"`, 1))
	if rec.Code != 200 {
		t.Fatalf("by name: %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(f.took(), ","); got != "kb" {
		t.Fatalf("by name asked %s, want kb", got)
	}
	// all failing: the one not resting tried, then of those resting only
	// the one back soonest (ka1, rested first, #1799), and the last
	// one's answer passed on
	f.set("kb", 429)
	rec = call("/v1/systemone", strings.Replace(ask, "%q", `"group/jevs"`, 1))
	if got := strings.Join(f.took(), ","); got != "kb,ka1" {
		t.Fatalf("all failing asked %s, want kb,ka1", got)
	}
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "no capacity for ka1") {
		t.Fatalf("all failing: %d %s", rec.Code, rec.Body)
	}
	// a chat request to it is told where it is asked
	rec = call("/v1/chat/completions", `{"model":"group/jevs","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "/v1/systemone") {
		t.Fatalf("chat to a decision group: %d %s", rec.Code, rec.Body)
	}
}

// A decision group as a chat group's classifier fails over the same way.
func TestDecisionGroupClassifierFailsOver(t *testing.T) {
	fresh(t)
	f := &decideFleet{status: map[string]int{"kc": 503}}
	a, b := f.serve(), f.serve()
	defer a.Close()
	defer b.Close()
	for _, p := range []provider.Provider{
		{ID: "jc", Name: "JC", Key: "kc", Decide: a.URL + "/v1"},
		{ID: "jd", Name: "JD", Key: "kd", Decide: b.URL + "/v1"},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := provider.SaveGroup(provider.Group{ID: "judges", Name: "Judges", Members: []string{"jc/jev-latest", "jd/jev-latest"}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	if v, err := s.askClassifier("group/judges", []string{"feature", "bug"}, before{}, false, "fix this"); err != nil || v.Intent != "bug" {
		t.Fatalf("classify: %+v %v", v, err)
	}
	asked := f.took()
	if len(asked) < 2 || asked[0] != "kc" || asked[len(asked)-1] != "kd" {
		t.Fatalf("asked %v", asked)
	}
	// jc rests: the next classification asks jd first
	if _, err := s.askClassifier("group/judges", []string{"feature", "bug"}, before{}, false, "fix this"); err != nil {
		t.Fatal(err)
	}
	if asked := f.took(); len(asked) == 0 || asked[0] != "kd" {
		t.Fatalf("after the rest asked %v", asked)
	}
}
