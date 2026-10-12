package gateway

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/provider"
)

// upstreamTLS is what the gateway's transports dial TLS with: nil, the
// system's roots, but for a test trusting its own server.
var upstreamTLS *tls.Config

// upstreamTransport is the gateway's transport to the vendors: a provider
// with a proxy of its own is sent through a transport kept for that proxy
// (#237). protocols nil speaks HTTP/2 wherever the server offers it.
func upstreamTransport(protocols *http.Protocols) http.RoundTripper {
	return netproxy.Dispatch(&http.Transport{
		Proxy:                 netproxy.Func,
		ResponseHeaderTimeout: 10 * time.Minute,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     protocols == nil,
		Protocols:             protocols,
		TLSClientConfig:       upstreamTLS,
	})
}

// codexHTTP1 sends what goes to the ChatGPT backend (provider.CodexBase's
// host, chatgpt.com) over HTTP/1.1, and the rest over next. Codex itself
// offers no ALPN at all and so speaks HTTP/1.1 to it, and Pi's own Codex
// provider a WebSocket or Node's fetch, HTTP/1.1 both; the gateway's
// HTTP/2 put every Codex stream, every account's, on one connection,
// where Cloudflare in front reset one now and then mid-reply ("stream
// error: stream ID 21; INTERNAL_ERROR; received from peer", jasonz3g on
// X: Codex in Pi through magpie, never Codex or Pi alone).
type codexHTTP1 struct{ next, h1 http.RoundTripper }

func newCodexHTTP1(next http.RoundTripper) codexHTTP1 {
	var h1 http.Protocols
	h1.SetHTTP1(true)
	return codexHTTP1{next: next, h1: upstreamTransport(&h1)}
}

func (t codexHTTP1) RoundTrip(r *http.Request) (*http.Response, error) {
	if u, err := url.Parse(provider.CodexBase); err == nil && r.URL.Host == u.Host {
		return t.h1.RoundTrip(r)
	}
	return t.next.RoundTrip(r)
}
