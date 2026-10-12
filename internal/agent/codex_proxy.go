package agent

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/netproxy"
)

// Codex sends its requests to magpie's gateway, http://127.0.0.1:3425, the
// way it sends every other: through a proxy when one is set (Muma102kMichael
// on X: every project and every new chat 502, "a local gateway config and a
// proxy switch, two pitfalls stacked"). Its HTTP client is reqwest with its
// default features (codex-rs Cargo.toml), whose proxy is hyper-util's
// Matcher::from_system: HTTP_PROXY/ALL_PROXY, else the system's proxy
// (Windows' Internet Options, macOS' network settings), and nothing goes
// around it but what NO_PROXY names — or, with no NO_PROXY, Windows'
// ProxyOverride — as an IP, a CIDR or "*" for an address: "127.*" and
// "localhost" there don't keep 127.0.0.1 off it, and macOS' exceptions
// aren't read at all. A proxy that sends 127.0.0.1 on to a server of its own
// (Clash in global mode, a rule set with no LAN-direct rule) answers 502,
// and Codex is down on every model, its own included once magpie carries
// them. With [features] respect_system_proxy (codex-rs outbound_proxy),
// Codex asks the system alone: Windows' bypass list is matched with its
// wildcards and <local>, macOS' exceptions likewise, and with no proxy set
// there it goes direct.
//
// So before magpie hands Codex's requests to the gateway uninvited (account
// failover), and while it has them, it asks the way Codex would go: through
// which proxy, and whether a request sent through it reaches magpie (Drift
// kind "proxied").

// codexProxyEnv, codexSystemProxy and codexProxyOS are what codexProxyFor
// reads; vars so tests can say.
var (
	codexProxyEnv    = os.Getenv
	codexSystemProxy = netproxy.System
	codexProxyOS     = runtime.GOOS
)

// codexProxyFor is the proxy this machine's Codex, configured by the
// config.toml at cfg, sends a request for base through, and where that
// proxy is set ("HTTP_PROXY", "the system's proxy settings"); "" when it
// goes straight there.
func codexProxyFor(cfg, base string) (proxy, from string) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "", ""
	}
	host := u.Hostname()
	env := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(codexProxyEnv(k)); v != "" {
				return v
			}
		}
		return ""
	}
	httpKeys := []string{"HTTP_PROXY", "http_proxy"}
	if u.Scheme == "https" {
		httpKeys = []string{"HTTPS_PROXY", "https_proxy"}
	}
	if f, _ := edit.GetTOMLTable(cfg, "features"); f["respect_system_proxy"] == "true" {
		// the system's alone, its own bypass list deciding: with no proxy
		// set there Codex goes direct, HTTP_PROXY or not (the environment's
		// count only when the system can't be read; tried with codex-cli
		// 0.162.1)
		sys := codexSystemProxy()
		if sys.URL == "" || systemBypassed(host, sys.Bypass) {
			return "", ""
		}
		return sys.URL, systemProxyName()
	}
	// reqwest's: each of the environment's first, the system's where it
	// names none, then ALL_PROXY; NO_PROXY, or Windows' bypass list without
	// it, goes around them
	p, k := env(httpKeys...), httpKeys[0]
	no := env("NO_PROXY", "no_proxy")
	sys := codexSystemProxy()
	if p == "" && sys.URL != "" {
		p, k = sys.URL, systemProxyName()
	}
	if p == "" {
		p, k = env("ALL_PROXY", "all_proxy"), "ALL_PROXY"
	}
	if no == "" && codexProxyOS == "windows" && sys.URL != "" {
		no = strings.ReplaceAll(strings.Join(sys.Bypass, ","), "*.", "")
	}
	if p == "" || reqwestNoProxy(host, no) {
		return "", ""
	}
	return p, k
}

func systemProxyName() string {
	if codexProxyOS == "windows" {
		return "Windows' proxy settings"
	}
	return "the system's proxy settings"
}

// reqwestNoProxy is hyper-util's NoProxy.contains: entries apart by commas;
// an address is matched only by an IP or a CIDR that holds it, a name by
// itself or a domain it is under; "*" matches every name, never an
// address.
func reqwestNoProxy(host, list string) bool {
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if ip != nil {
			if _, n, err := net.ParseCIDR(e); err == nil && n.Contains(ip) {
				return true
			}
			if x := net.ParseIP(e); x != nil && x.Equal(ip) {
				return true
			}
			continue
		}
		d := strings.TrimPrefix(e, ".")
		if e == "*" || strings.EqualFold(host, d) || strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(d)) {
			return true
		}
	}
	return false
}

// systemBypassed is Codex's own match of a host against the system's bypass
// list (codex-rs outbound_proxy/windows.rs): <local> is a name with no dot,
// an entry with * a wildcard, any other the host itself or, with a leading
// dot, a domain it is under.
func systemBypassed(host string, bypass []string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	for _, e := range bypass {
		e = strings.ToLower(strings.TrimSpace(e))
		e = strings.TrimPrefix(strings.TrimPrefix(e, "http://"), "https://")
		switch {
		case e == "":
		case e == "<local>":
			if !strings.Contains(host, ".") {
				return true
			}
		case e == "*":
			return true
		case strings.HasPrefix(e, "."):
			if host == e[1:] || strings.HasSuffix(host, e) {
				return true
			}
		case strings.Contains(e, "*"):
			if ok, _ := path.Match(e, host); ok {
				return true
			}
		case host == e:
			return true
		}
	}
	return false
}

// proxyProbe says whether a request for base sent through proxy is answered
// by magpie: its gateway's GET / names it. A proxy's own 502, or anything
// else that isn't magpie, is not. A var for tests.
var proxyProbe = realProxyProbe

func realProxyProbe(base, proxy string) bool {
	p, err := netproxy.Parse(proxy)
	if err != nil {
		return false
	}
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(p)}}
	res, err := c.Get(strings.TrimSuffix(base, "/") + "/")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var info struct {
		Name string `json:"name"`
	}
	return res.StatusCode == http.StatusOK && json.NewDecoder(res.Body).Decode(&info) == nil && info.Name == "magpie"
}

// codexThroughProxy says whether Codex, configured by cfg, gets to the
// gateway at base: true when it goes there directly, or through a proxy
// that is known to pass its requests on. known is false while that proxy
// hasn't been tried yet; a try is started then (see reachable).
func codexThroughProxy(cfg, base string) (ok, known bool, proxy, from string) {
	proxy, from = codexProxyFor(cfg, base)
	if proxy == "" {
		return true, true, "", ""
	}
	ok, known = probed("proxy "+proxy+" "+base, func() bool { return proxyProbe(base, proxy) })
	return ok, known, proxy, from
}

// codexProxyOK is codexThroughProxy waited for: whether Codex gets to the
// gateway at base, a proxy in its way tried now when it hasn't been.
func codexProxyOK(cfg, base string) bool {
	proxy, _ := codexProxyFor(cfg, base)
	return proxy == "" || probedNow("proxy "+proxy+" "+base, func() bool { return proxyProbe(base, proxy) })
}

// codexProxiedDetail says what keeps Codex off magpie through proxy, and
// how to get it there.
func codexProxiedDetail(base, proxy, from string) string {
	host := strings.TrimPrefix(base, "http://")
	d := "Codex sends its requests for magpie (" + host + ") through the proxy at " + proxy + ", set in " + from + ", and that proxy doesn't pass them on to magpie: every turn fails, with 502 Bad Gateway, whichever model it is on. " +
		"Have the proxy send 127.0.0.1 direct (a DIRECT rule for 127.0.0.0/8, or rule mode rather than global), "
	switch {
	case strings.Contains(from, "PROXY"):
		d += "or add 127.0.0.1 to NO_PROXY, "
	case codexProxyOS == "windows":
		d += "or add 127.0.0.1 to \"Don't use the proxy server for addresses beginning with\" in Windows' proxy settings (127.* there is not enough for Codex), "
	default:
		d += "or set NO_PROXY=127.0.0.1 for Codex, "
	}
	return d + "then quit and reopen Codex. Until then, Disconnect from magpie puts Codex's config back as it was."
}
