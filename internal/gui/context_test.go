package gui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sessions"
)

// ctxRoute is a done request of agent in session whose prompt holds base
// (system prompt, tools, memory) and chat, the cache read of it counted.
func ctxRoute(id int64, agent, session string, base, chat, window, cached int) gateway.Route {
	return gateway.Route{
		ID: id, Time: time.Unix(1_790_000_000+id*60, 0), Agent: agent, Session: session, Model: agent + "-model", Done: true, Status: 200,
		Usage: []gateway.RouteUsage{{CacheRead: cached}},
		Prompt: &gateway.Prompt{Window: window, Tokens: base + chat, Counted: true, Parts: []gateway.PromptPart{
			{Kind: gateway.PartSystem, Tokens: base / 2},
			{Kind: gateway.PartTools, Tokens: base - base/2, Items: []gateway.PromptItem{{Name: "linear", Tag: "mcp", Tokens: base * 2 / 5}}},
			{Kind: gateway.PartChat, Tokens: chat},
		}},
	}
}

func tagKeys(a ctxAgent) []string {
	var keys []string
	for _, tg := range a.Tags {
		keys = append(keys, tg.Key)
	}
	return keys
}

// An agent that starts small, grows slowly and is read from the cache
// scores high and is told lean and cache-friendly; its session's line is
// its requests', oldest first.
func TestContextOfALeanAgent(t *testing.T) {
	var routes []gateway.Route
	for i, chat := range []int{1000, 3000, 6000, 8000} {
		total := 12000 + chat
		routes = append(routes, ctxRoute(int64(i+1), "codex", "s1", 12000, chat, 272000, total*9/10))
	}
	// a title call is no part of what its prompts are like
	title := ctxRoute(9, "codex", "s1", 500, 0, 272000, 0)
	title.Kind = "title"
	routes = append(routes, title)

	out := contextOf(routes, 7)
	if len(out.Agents) != 1 || len(out.Sessions) != 1 {
		t.Fatalf("agents %d sessions %d", len(out.Agents), len(out.Sessions))
	}
	a := out.Agents[0]
	if a.Requests != 4 || a.Baseline != 12000 || a.Window != 272000 {
		t.Fatalf("requests %d baseline %d window %d", a.Requests, a.Baseline, a.Window)
	}
	if a.Score < 90 {
		t.Fatalf("score %d %+v", a.Score, a.Scores)
	}
	for _, want := range []string{"lean", "cache-friendly"} {
		if !slices.Contains(tagKeys(a), want) {
			t.Fatalf("tags %v, want %s", tagKeys(a), want)
		}
	}
	s := out.Sessions[0]
	if s.Requests != 4 || s.Peak != 20000 || s.LatestID != 4 || len(s.Points) != 4 || s.Points[0].Tokens != 13000 {
		t.Fatalf("session %+v", s)
	}
}

// One that starts with most of a window of MCP tools, misses the cache,
// compacts and fails often scores low and is told so.
func TestContextOfAHeavyAgent(t *testing.T) {
	var routes []gateway.Route
	chats := []int{10000, 40000, 100000, 5000, 20000, 30000}
	for i, chat := range chats {
		routes = append(routes, ctxRoute(int64(i+1), "claude", "s1", 70000, chat, 200000, 1000))
	}
	for i := range 3 {
		routes = append(routes, gateway.Route{ID: int64(20 + i), Time: time.Unix(1_790_000_000, 0), Agent: "claude", Done: true, Status: 529})
	}
	a := contextOf(routes, 7).Agents[0]
	if a.Compacts != 1 || a.Errors != 3 || a.Calls != 9 {
		t.Fatalf("compacts %d errors %d calls %d", a.Compacts, a.Errors, a.Calls)
	}
	if a.Score >= 50 {
		t.Fatalf("score %d %+v", a.Score, a.Scores)
	}
	for _, want := range []string{"cache-misses", "heavy-start", "mcp-heavy", "compacts", "flaky", "near-full"} {
		if !slices.Contains(tagKeys(a), want) {
			t.Fatalf("tags %v, want %s", tagKeys(a), want)
		}
	}
}

// An agent whose vendor doesn't count the cache is scored neither up nor
// down for it, and not told cache-friendly or cache-misses.
func TestContextCacheNotKnown(t *testing.T) {
	var routes []gateway.Route
	for i := range 6 {
		r := ctxRoute(int64(i+1), "gemini", "s1", 8000, 1000*i, 1000000, 0)
		r.Prompt.Counted, r.Usage = false, nil
		routes = append(routes, r)
	}
	a := contextOf(routes, 1).Agents[0]
	if a.Scores[0].Key != "cache" || a.Scores[0].Points != 18 {
		t.Fatalf("cache score %+v", a.Scores[0])
	}
	for _, k := range tagKeys(a) {
		if k == "cache-friendly" || k == "cache-misses" {
			t.Fatalf("tags %v", tagKeys(a))
		}
	}
}

// A session goes by the name the Sessions page shows for it: here the one
// a Claude Code session was renamed to.
func TestContextSessionTitles(t *testing.T) {
	h := sandboxHome(t)
	claude := filepath.Join(h, ".claude")
	proj := filepath.Join(claude, "projects", "-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-2222-3333-4444-555555555555"
	body := `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"hi"},"uuid":"u","timestamp":"2026-09-20T10:00:00.000Z","cwd":"/work","sessionId":"` + id + `"}` + "\n" +
		`{"parentUuid":"u","isSidechain":false,"message":{"model":"claude-sonnet-5","id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":10,"output_tokens":2}},"requestId":"req_1","type":"assistant","uuid":"a","timestamp":"2026-09-20T10:00:01.000Z","cwd":"/work","sessionId":"` + id + `"}` + "\n" +
		`{"type":"custom-title","customTitle":"Port the parser","sessionId":"` + id + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	sessions.Reset()
	t.Cleanup(sessions.Reset)

	out := contextOf([]gateway.Route{
		ctxRoute(1, "claude", id, 20000, 1000, 200000, 0),
		ctxRoute(2, "claude", "not-a-session", 20000, 1000, 200000, 0),
	}, 7)
	titles := map[string]string{}
	for _, s := range out.Sessions {
		titles[s.Key] = s.Title
	}
	if titles[id] != "Port the parser" || titles["not-a-session"] != "" {
		t.Fatalf("titles %v", titles)
	}
}

// masaka on Discord: a part of the context window card opens to its text.
// /api/context/text lists the part's items, and gives one item's text when
// asked for it; a request whose body is no longer kept is 404.
func TestContextTextAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":90,"output_tokens":1}}`)
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "ct", Name: "CT", Anthropic: up.URL, Models: []string{"m1"}, Key: "k"}); err != nil {
		t.Fatal(err)
	}
	gw := gateway.New()
	prev := served.Swap(gw)
	t.Cleanup(func() { served.Store(prev) })
	body := `{"model":"ct/m1","max_tokens":10,"system":"You are the reviewer agent.",
		"tools":[{"name":"Read","description":"Reads a file.","input_schema":{"type":"object"}},{"name":"Grep","description":"Searches files for a long pattern, across the whole tree.","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"review it"}]}`
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	rs := gw.Trace(context.Background(), 0, 0).Routes
	if rec.Code != 200 || len(rs) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	mux := http.NewServeMux()
	contextRoutesAPI(mux)
	get := func(q string) (int, gateway.PromptText) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/context/text?"+q, nil))
		var pt gateway.PromptText
		json.Unmarshal(rec.Body.Bytes(), &pt)
		return rec.Code, pt
	}
	id := strconv.FormatInt(rs[0].ID, 10)
	code, list := get("id=" + id + "&kind=tools")
	if code != 200 || len(list.Items) != 2 || list.Items[0].Name != "Grep" || list.Items[0].Pieces != nil || list.Items[1].Pieces != nil {
		t.Fatalf("%d %+v", code, list)
	}
	code, one := get("id=" + id + "&kind=tools&item=1")
	if code != 200 || one.Items[0].Pieces != nil || len(one.Items[1].Pieces) != 1 || !strings.Contains(one.Items[1].Pieces[0].Text, "Reads a file.") {
		t.Fatalf("%d %+v", code, one)
	}
	if code, sys := get("id=" + id + "&kind=system&item=0"); code != 200 || sys.Items[0].Pieces[0].Text != "You are the reviewer agent." {
		t.Fatalf("%d %+v", code, sys)
	}
	if code, _ := get("id=1&kind=tools"); code != 404 {
		t.Fatalf("not kept: %d", code)
	}
	if code, _ := get("id=" + id + "&kind=tools&item=9"); code != 400 {
		t.Fatalf("no such item: %d", code)
	}
}
