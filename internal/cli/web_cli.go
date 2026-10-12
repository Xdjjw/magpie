package cli

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/gui"
)

// webAddr is where `magpie web` listens unless told: beside the gateway.
const webAddr = "127.0.0.1:3430"

// webCmd: magpie web [--addr host:port] [--lan] [--no-open] [--gateway] —
// the app's window in a browser, for a computer that can't show the app;
// --gateway shows it in gateway mode (gui.WebGateway) unless Settings says
// otherwise.
func webCmd(args []string) error {
	addr, lan, open := webAddr, false, true
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--addr" && i+1 < len(args):
			i++
			addr = args[i]
		case strings.HasPrefix(a, "--addr="):
			addr = strings.TrimPrefix(a, "--addr=")
		case a == "--lan":
			lan = true
		case a == "--no-open":
			open = false
		case a == "--gateway":
			gui.WebGateway.Store(true)
		default:
			return fmt.Errorf("magpie web: unknown %q · magpie web [--addr host:port] [--lan] [--no-open] [--gateway], MAGPIE_WEB_KEY to keep one key", a)
		}
	}
	if lan && addr == webAddr {
		_, port, _ := net.SplitHostPort(webAddr)
		addr = "0.0.0.0:" + port
	}
	w, err := gui.StartWeb(addr, version)
	if err != nil {
		return err
	}
	fmt.Println(green.Render("●"), "magpie web on", bold.Render(w.Link))
	host, port, _ := net.SplitHostPort(w.Addr)
	key := w.Link[strings.Index(w.Link, "/?k="):]
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		for _, l := range gui.NetworkLinks(port, key) {
			fmt.Println(muted.Render("  on the network"), l)
		}
		if gateway.ContainerAddrs() {
			fmt.Println(muted.Render("  " + containerNote))
		}
		fmt.Println(amber.Render("!"), "anyone with the link can change magpie and see its keys, and the network carries it unencrypted")
	} else if l := proxyLink(key); l != "" {
		fmt.Println(muted.Render("  through your proxy"), l)
	}
	carries := "this run's key (MAGPIE_WEB_KEY keeps one across runs)"
	if os.Getenv("MAGPIE_WEB_KEY") != "" {
		carries = "MAGPIE_WEB_KEY"
	}
	fmt.Println(muted.Render("  the link carries " + carries + " · gateway " + advertisedURL() + " · Ctrl-C to stop"))
	if open {
		openInBrowser(w.Link)
	}
	return w.Wait()
}

// proxyLink is the page's link through a reverse proxy in front of a page
// served on loopback (#1479), with the key: MAGPIE_WEB_URL, the page's own
// address there (https://magpie.example.com), or else MAGPIE_PUBLIC_URL
// with no port of its own, as NetworkLinks takes it (#372). One with a
// port names a host whose ports are published, not a proxy. "" with
// neither, or a MAGPIE_WEB_URL that isn't a plain http(s) address.
func proxyLink(key string) string {
	if v := strings.TrimSpace(os.Getenv("MAGPIE_WEB_URL")); v != "" {
		if !strings.Contains(v, "://") {
			v = "https://" + v
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return ""
		}
		return strings.TrimRight(u.String(), "/") + key
	}
	u, err := url.Parse(gateway.PublicURL())
	if err != nil || u.Host == "" || u.Port() != "" {
		return ""
	}
	return strings.TrimRight(u.String(), "/") + key
}
