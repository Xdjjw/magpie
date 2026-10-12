package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/provider"
)

// proxyAs has codexProxyFor read env, sys and goos, and proxyProbe answer
// with *works, until the test ends.
func proxyAs(t *testing.T, env map[string]string, sys netproxy.Proxy, goos string, works *bool) {
	t.Helper()
	e, s, o, p := codexProxyEnv, codexSystemProxy, codexProxyOS, proxyProbe
	codexProxyEnv = func(k string) string { return env[k] }
	codexSystemProxy = func() netproxy.Proxy { return sys }
	codexProxyOS = goos
	proxyProbe = func(string, string) bool { return works != nil && *works }
	reachForget()
	t.Cleanup(func() {
		codexProxyEnv, codexSystemProxy, codexProxyOS, proxyProbe = e, s, o, p
		reachForget()
	})
}

// Which proxy Codex sends a request for magpie's 127.0.0.1 through, the way
// its reqwest does (hyper-util's Matcher::from_system) and, with
// respect_system_proxy on, the way codex-rs outbound_proxy does.
func TestCodexProxyFor(t *testing.T) {
	const base = "http://127.0.0.1:3425/v1"
	clash := "http://127.0.0.1:7890"
	plain := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(plain, []byte("model = \"gpt-5.5\"\n"), 0o600)
	respect := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(respect, []byte("model = \"gpt-5.5\"\n\n[features]\nrespect_system_proxy = true\n"), 0o600)
	win := netproxy.Proxy{URL: clash, Bypass: []string{"localhost", "127.*", "<local>"}}
	cases := []struct {
		name string
		cfg  string
		env  map[string]string
		sys  netproxy.Proxy
		goos string
		want string
	}{
		{"none", plain, nil, netproxy.Proxy{}, "darwin", ""},
		{"HTTP_PROXY", plain, map[string]string{"HTTP_PROXY": clash}, netproxy.Proxy{}, "darwin", clash},
		{"https_proxy only", plain, map[string]string{"HTTPS_PROXY": clash}, netproxy.Proxy{}, "darwin", ""},
		{"ALL_PROXY", plain, map[string]string{"ALL_PROXY": clash}, netproxy.Proxy{}, "linux", clash},
		{"NO_PROXY 127.0.0.1", plain, map[string]string{"HTTP_PROXY": clash, "NO_PROXY": "localhost,127.0.0.1"}, netproxy.Proxy{}, "darwin", ""},
		{"NO_PROXY CIDR", plain, map[string]string{"HTTP_PROXY": clash, "NO_PROXY": "127.0.0.0/8"}, netproxy.Proxy{}, "darwin", ""},
		{"NO_PROXY localhost is not 127.0.0.1", plain, map[string]string{"HTTP_PROXY": clash, "NO_PROXY": "localhost"}, netproxy.Proxy{}, "darwin", clash},
		{"NO_PROXY * is names only", plain, map[string]string{"HTTP_PROXY": clash, "NO_PROXY": "*"}, netproxy.Proxy{}, "darwin", clash},
		// the reporter's: Windows' proxy on, with 127.* in its bypass list,
		// which reqwest doesn't read as 127.0.0.1
		{"Windows 127.*", plain, nil, win, "windows", clash},
		{"Windows 127.0.0.1", plain, nil, netproxy.Proxy{URL: clash, Bypass: []string{"127.0.0.1"}}, "windows", ""},
		{"Windows NO_PROXY wins over its list", plain, map[string]string{"NO_PROXY": "example.com"}, netproxy.Proxy{URL: clash, Bypass: []string{"127.0.0.1"}}, "windows", clash},
		{"macOS exceptions not read", plain, nil, netproxy.Proxy{URL: clash, Bypass: []string{"127.0.0.1"}}, "darwin", clash},
		{"respect: Windows 127.* goes around", respect, nil, win, "windows", ""},
		{"respect: system proxy", respect, nil, netproxy.Proxy{URL: clash, Bypass: []string{"*.local"}}, "darwin", clash},
		// the system says direct, and the environment isn't asked (codex-cli
		// 0.162.1 with HTTP_PROXY set sent nothing to it)
		{"respect: no system proxy, env not read", respect, map[string]string{"HTTP_PROXY": clash}, netproxy.Proxy{}, "darwin", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proxyAs(t, c.env, c.sys, c.goos, nil)
			if got, from := codexProxyFor(c.cfg, base); got != c.want || (got != "" && from == "") {
				t.Fatalf("got %q from %q, want %q", got, from, c.want)
			}
		})
	}
}

// A proxy's own 502 isn't magpie; a proxy that passes the request on is.
func TestRealProxyProbe(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"magpie"}`)
	}))
	defer gw.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream", http.StatusBadGateway)
	}))
	defer dead.Close()
	pass := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.Parse(r.RequestURI)
		res, err := http.Get(u.String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
	}))
	defer pass.Close()
	if realProxyProbe(gw.URL+"/v1", dead.URL) {
		t.Fatal("a proxy answering 502 counted as reaching magpie")
	}
	if !realProxyProbe(gw.URL+"/v1", pass.URL) {
		t.Fatal("a proxy passing the request on didn't count")
	}
}

// Muma102kMichael on X: Codex 502 on every project and every new chat once
// magpie took it over, its requests for 127.0.0.1 sent through a proxy that
// didn't pass them on. Failover, which magpie writes uninvited, is never
// written then, and one written before goes again; once the proxy passes
// them on, it is.
func TestCodexFailoverHeldWhileProxyDoesNotPassOn(t *testing.T) {
	claims := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
	}
	auth := func(email, acct string) map[string]any {
		return map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
			"id_token":      claims(map[string]any{"email": email}),
			"access_token":  claims(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}),
			"refresh_token": "r-" + acct, "account_id": acct}}
	}
	me, _ := json.Marshal(auth("me@example.com", "acct-1"))
	home, read := codexHome(t, string(me), "model = \"gpt-5.5\"\n")
	b, _ := json.Marshal([]map[string]any{{"agent": "codex", "user": "spare@example.com", "on": true,
		"seen": time.Now(), "auth": auth("spare@example.com", "acct-2")}})
	os.MkdirAll(filepath.Dir(provider.Path()), 0o755)
	os.WriteFile(filepath.Join(filepath.Dir(provider.Path()), "logins.json"), b, 0o600)

	works := false
	proxyAs(t, nil, netproxy.Proxy{URL: "http://127.0.0.1:7890", Bypass: []string{"localhost", "127.*"}}, "windows", &works)
	cx := codex(home)
	sync := func() string {
		t.Helper()
		reachForget()
		if err := cx.Sync(); err != nil {
			t.Fatal(err)
		}
		return read()
	}
	if cfg := sync(); strings.Contains(cfg, "openai_base_url") {
		t.Fatalf("written while the proxy doesn't pass it on:\n%s", cfg)
	}
	works = true
	if cfg := sync(); !strings.Contains(cfg, `openai_base_url = "`+gateway.URL()) {
		t.Fatalf("not written once the proxy passes it on:\n%s", cfg)
	}
	works = false
	if cfg := sync(); strings.Contains(cfg, "openai_base_url") || !strings.Contains(cfg, `model = "gpt-5.5"`) {
		t.Fatalf("kept once the proxy stopped passing it on:\n%s", cfg)
	}
}

// Connected to magpie, with the gateway up but Codex's way to it through a
// proxy that answers 502: the row says so, which proxy and where it is set,
// rather than connected (kind "proxied"); once the proxy passes it on, or
// 127.0.0.1 goes around it, connected again.
func TestCodexProxiedDrift(t *testing.T) {
	fakeReach(t, map[string]bool{gateway.URL(): true})
	home, _ := codexAt(t, gateway.URL())
	works := false
	env := map[string]string{"HTTP_PROXY": "http://127.0.0.1:7890"}
	proxyAs(t, env, netproxy.Proxy{}, "darwin", &works)

	d := driftSettled(t, codex(home))
	if d == nil || d.Kind != "proxied" || d.Addr != "http://127.0.0.1:7890" {
		t.Fatalf("drift %+v", d)
	}
	if !strings.Contains(d.Detail, "HTTP_PROXY") || !strings.Contains(d.Detail, "NO_PROXY") || !strings.Contains(d.Detail, "Disconnect") {
		t.Fatalf("detail: %s", d.Detail)
	}
	works = true
	reachForget()
	if d := driftSettled(t, codex(home)); d != nil {
		t.Fatalf("proxy passes it on: %+v", d)
	}
	works = false
	env["NO_PROXY"] = "127.0.0.1"
	reachForget()
	if d := driftSettled(t, codex(home)); d != nil {
		t.Fatalf("127.0.0.1 around the proxy: %+v", d)
	}
}

// Leon920717 on X: with magpie quit, Codex CLI kept retrying the gateway
// till magpie was open again. The failover base URL magpie wrote goes as
// the magpie serving the gateway quits (EndCodexFailover), and the next
// start's Sync writes it again; a base URL the user wrote stays.
func TestCodexFailoverEndsWithMagpie(t *testing.T) {
	claims := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
	}
	auth := func(email, acct string) map[string]any {
		return map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
			"id_token":      claims(map[string]any{"email": email}),
			"access_token":  claims(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}),
			"refresh_token": "r-" + acct, "account_id": acct}}
	}
	me, _ := json.Marshal(auth("me@example.com", "acct-1"))
	home, read := codexHome(t, string(me), "model = \"gpt-5.5\"\n")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	logins := func(on bool) {
		b, _ := json.Marshal([]map[string]any{{"agent": "codex", "user": "spare@example.com", "on": on,
			"seen": time.Now(), "auth": auth("spare@example.com", "acct-2")}})
		os.MkdirAll(filepath.Dir(provider.Path()), 0o755)
		os.WriteFile(filepath.Join(filepath.Dir(provider.Path()), "logins.json"), b, 0o600)
	}
	logins(true)
	cx := codex(home)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !strings.Contains(cfg, "openai_base_url") {
		t.Fatalf("failover not written:\n%s", cfg)
	}
	wasKeep := keepCodexDaemon
	t.Cleanup(func() { keepCodexDaemon = wasKeep })
	var asked []string
	keepCodexDaemon = func(_ context.Context, h string, _ time.Time) provider.CodexDaemonCheck {
		asked = append(asked, h)
		return provider.CodexDaemonCheck{}
	}
	if err := EndCodexFailover(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); strings.Contains(cfg, "openai_base_url") || !strings.Contains(cfg, `model = "gpt-5.5"`) {
		t.Fatalf("magpie quit, Codex still pointed at it:\n%s", cfg)
	}
	// the app-server keeps the base URL it started with: asked to restart
	if len(asked) != 1 || asked[0] != filepath.Dir(cx.Path) {
		t.Fatalf("app-server not brought up to date on quit: %q", asked)
	}
	asked = nil
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !strings.Contains(cfg, "openai_base_url") {
		t.Fatalf("not written again on the next start:\n%s", cfg)
	}

	// the user's own base URL, with no failover on, stays
	logins(false)
	if err := cx.Sync(); err != nil {
		t.Fatal(err)
	}
	base := gateway.URL() + gateway.CodexPath
	if err := edit.SetTOMLTop(cx.Path, edit.KV{Path: "openai_base_url", Value: base}); err != nil {
		t.Fatal(err)
	}
	if err := EndCodexFailover(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); !strings.Contains(cfg, `openai_base_url = "`+base+`"`) {
		t.Fatalf("the user's base URL was taken:\n%s", cfg)
	}
	if len(asked) != 0 {
		t.Fatalf("app-server restarted with nothing changed: %q", asked)
	}
}

// CC Switch with "unify Codex session history" on (xuxing0727 on X) runs
// the official sign-in as its "custom" mirror, so failover goes on that
// table's base_url rather than openai_base_url: it too waits for the proxy
// to pass Codex on, and goes as the magpie serving the gateway quits.
func TestCodexMirrorFailoverHeldAndEndsWithMagpie(t *testing.T) {
	// CC Switch's own bytes (src-tauri services/provider/mod.rs)
	const unified = "model_provider = \"custom\"\nmodel = \"gpt-5.5\"\n\n" +
		"[model_providers.custom]\nname = \"OpenAI\"\nrequires_openai_auth = true\nsupports_websockets = true\nwire_api = \"responses\"\n"
	claims := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
	}
	auth := func(email, acct string) map[string]any {
		return map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
			"id_token":      claims(map[string]any{"email": email}),
			"access_token":  claims(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}),
			"refresh_token": "r-" + acct, "account_id": acct}}
	}
	me, _ := json.Marshal(auth("me@example.com", "acct-1"))
	home, read := codexHome(t, string(me), unified)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	b, _ := json.Marshal([]map[string]any{{"agent": "codex", "user": "spare@example.com", "on": true,
		"seen": time.Now(), "auth": auth("spare@example.com", "acct-2")}})
	os.MkdirAll(filepath.Dir(provider.Path()), 0o755)
	os.WriteFile(filepath.Join(filepath.Dir(provider.Path()), "logins.json"), b, 0o600)
	wasKeep := keepCodexDaemon
	t.Cleanup(func() { keepCodexDaemon = wasKeep })
	keepCodexDaemon = func(context.Context, string, time.Time) provider.CodexDaemonCheck { return provider.CodexDaemonCheck{} }

	works := false
	proxyAs(t, map[string]string{"HTTP_PROXY": "http://127.0.0.1:7890"}, netproxy.Proxy{}, "darwin", &works)
	cx := codex(home)
	sync := func() string {
		t.Helper()
		reachForget()
		if err := cx.Sync(); err != nil {
			t.Fatal(err)
		}
		return read()
	}
	if cfg := sync(); strings.Contains(cfg, "base_url") {
		t.Fatalf("written while the proxy doesn't pass it on:\n%s", cfg)
	}
	works = true
	if cfg := sync(); !strings.Contains(cfg, `base_url = "`+gateway.URL()) {
		t.Fatalf("not written once the proxy passes it on:\n%s", cfg)
	}
	if err := EndCodexFailover(); err != nil {
		t.Fatal(err)
	}
	if cfg := read(); strings.Contains(cfg, "base_url") || !strings.Contains(cfg, `model_provider = "custom"`) || !strings.Contains(cfg, "supports_websockets = true") {
		t.Fatalf("magpie quit, CC Switch's mirror still pointed at it:\n%s", cfg)
	}
}
