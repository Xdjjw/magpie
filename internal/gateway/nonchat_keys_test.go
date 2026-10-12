package gateway

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// keyedVendor answers images and embeddings on any key, and keeps which
// key each path was asked on.
type keyedVendor struct {
	mu  sync.Mutex
	got map[string][]string
}

func (v *keyedVendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	v.mu.Lock()
	if v.got == nil {
		v.got = map[string][]string{}
	}
	v.got[r.URL.Path] = append(v.got[r.URL.Path], strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/images/generations":
		io.WriteString(w, `{"created":1,"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString(pngBytes)+`"}]}`)
	case "/v1/embeddings":
		io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
	default:
		w.WriteHeader(404)
	}
}

func (v *keyedVendor) keys(path string) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.got[path]...)
}

// keyedRelay is a provider of two keys: the first set to serve the chat
// model only, the second the image and embedding models only (#474).
func keyedRelay(t *testing.T) (*Server, *keyedVendor) {
	t.Helper()
	fresh(t)
	v := &keyedVendor{}
	up := httptest.NewServer(v)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: up.URL + "/v1", Key: "key-a", Keys: []provider.KeyAccount{{Key: "key-b"}},
		Models: []string{"text", "gpt-image-2", "text-embedding-3-small"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetAccountModels("relay", provider.KeyID("key-a"), []string{"text"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetAccountModels("relay", provider.KeyID("key-b"), []string{"gpt-image-2", "text-embedding-3-small"}); err != nil {
		t.Fatal(err)
	}
	return New(), v
}

func postJSON(t *testing.T, s *Server, path, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// An image or an embedding goes to the key whose own list of models has
// it, as a chat does, never the first key, set to serve another (#1657,
// wangru8080); with every key set not to serve it, it is refused with why.
func TestNonChatGoesToTheKeyThatServesIt(t *testing.T) {
	s, v := keyedRelay(t)
	if code, raw := postJSON(t, s, "/v1/images/generations", `{"model":"relay/gpt-image-2","prompt":"a magpie"}`); code != 200 {
		t.Fatalf("draw: %d %s", code, raw)
	}
	if got := v.keys("/v1/images/generations"); len(got) != 1 || got[0] != "key-b" {
		t.Fatalf("drawn on %v, want key-b alone", got)
	}
	if code, raw := postJSON(t, s, "/v1/embeddings", `{"model":"relay/text-embedding-3-small","input":"a magpie"}`); code != 200 {
		t.Fatalf("embed: %d %s", code, raw)
	}
	if got := v.keys("/v1/embeddings"); len(got) != 1 || got[0] != "key-b" {
		t.Fatalf("embedded on %v, want key-b alone", got)
	}

	// key-b taken off the image model: no key serves it, and nothing is sent
	if err := provider.SetAccountModels("relay", provider.KeyID("key-b"), []string{"text-embedding-3-small"}); err != nil {
		t.Fatal(err)
	}
	if code, raw := postJSON(t, s, "/v1/images/generations", `{"model":"relay/gpt-image-2","prompt":"a magpie"}`); code != 403 || !strings.Contains(raw, "gpt-image-2") {
		t.Fatalf("draw on no key: %d %s", code, raw)
	}
	if err := provider.SetAccountModels("relay", provider.KeyID("key-b"), []string{"gpt-image-2"}); err != nil {
		t.Fatal(err)
	}
	if code, raw := postJSON(t, s, "/v1/embeddings", `{"model":"relay/text-embedding-3-small","input":"a magpie"}`); code != 403 || !strings.Contains(raw, "text-embedding-3-small") {
		t.Fatalf("embed on no key: %d %s", code, raw)
	}
	if n, m := len(v.keys("/v1/images/generations")), len(v.keys("/v1/embeddings")); n != 1 || m != 1 {
		t.Fatalf("a key set not to serve the model was asked: %d draws, %d embeddings", n, m)
	}
}

// A video at a provider's own videos API starts on the key whose own list
// of models has it, and is asked after on that key (#1657).
func TestCustomVideoGoesToTheKeyThatServesIt(t *testing.T) {
	fresh(t)
	up := newOpenAIVideos(t)
	saveFetched(t, provider.Provider{ID: "studio", Name: "Studio", Chat: up.URL + "/v1", Key: "kA", Keys: []provider.KeyAccount{{Key: "kB"}}})
	if err := provider.SetAccountModels("studio", provider.KeyID("kA"), []string{"gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetAccountModels("studio", provider.KeyID("kB"), []string{"vid-1"}); err != nil {
		t.Fatal(err)
	}
	h := New().Handler()
	code, obj, raw := asKey(t, h, "", "POST", "/v1/videos", `{"model":"studio/vid-1","prompt":"a cat"}`)
	id, _ := obj["id"].(string)
	if code != 200 || id == "" {
		t.Fatalf("start: %d %s", code, raw)
	}
	if code, _, raw = asKey(t, h, "", "GET", "/v1/videos/"+id, ""); code != 200 {
		t.Fatalf("poll: %d %s", code, raw)
	}
	for _, a := range up.log()[1:] { // [0] is the model list's fetch
		if !strings.HasSuffix(a, " kB") {
			t.Fatalf("asked on another key than kB: %v", up.log())
		}
	}
	if err := provider.SetAccountModels("studio", provider.KeyID("kB"), []string{"gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	before := len(up.log())
	if code, _, raw = asKey(t, h, "", "POST", "/v1/videos", `{"model":"studio/vid-1","prompt":"a cat"}`); code != 403 {
		t.Fatalf("start on no key: %d %s", code, raw)
	}
	if len(up.log()) != before {
		t.Fatalf("a key set not to serve vid-1 was asked: %v", up.log()[before:])
	}
}
