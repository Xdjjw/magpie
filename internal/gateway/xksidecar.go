package gateway

// 补丁 3:sidecar 生命周期。
//
// xkeysmith 的核心(语义平面)已经**内置**在本 fork 里(见 xk/ 与 xkattach.go),
// 所以多数情况下不需要外部进程。但「外部 xkeysmith 网关」仍是受支持的部署:
// 有些人用官方发布的二进制/桌面端,想让 magpie 复用它。
//
// 这里的职责因此很小:当配了 XKEYSMITH_BIN 时,若目标端口没有健康的
// xkeysmith,就把它拉起来;进程随网关退出而回收。健康检查用 /healthz
// 的 service 字段(与桌面端探活同一个约定),**不把「进程存活」当连通证明**。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
)

// xkSidecar 管理外部 xkeysmith 进程(可选)。
type xkSidecar struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	baseURL string
	// checked 记住「这个 base URL 已经探过/拉起过」,避免在每个请求的
	// 热路径上反复发探活请求(那既拖慢请求,也会干扰上游调用的时序)。
	checked bool
}

func newXkSidecar() *xkSidecar { return &xkSidecar{} }

// Ensure 在需要时拉起外部 xkeysmith。返回它是否已经健康。
//
// 只在**首次**见到这个 base URL 时探活/拉起;之后直接放行,热路径零开销。
// 只对本机回环地址动作:配了远端/自定义 base 的 provider 不归 sidecar 管,
// 也绝不能对它发探活请求(那会污染上游的调用时序)。
func (s *xkSidecar) Ensure(baseURL string) bool {
	if !isLoopbackURL(baseURL) {
		return false
	}
	// 只对 xkeysmith 自己的默认端口动作。provider 指向别的回环地址(另一个
	// 本地服务、或测试里的假上游)时不探活——探活会白白消耗它的请求。
	if !xkIsDefaultEndpoint(baseURL) && strings.TrimSpace(os.Getenv("XKEYSMITH_BIN")) == "" {
		return false
	}
	s.mu.Lock()
	if s.checked && s.baseURL == baseURL {
		s.mu.Unlock()
		return s.cmd != nil
	}
	s.mu.Unlock()

	// 先探活:已经有健康的实例(例如用户自己起的桌面端)就什么都不做。
	if xkHealthy(baseURL) {
		s.mu.Lock()
		s.checked, s.baseURL = true, baseURL
		s.mu.Unlock()
		return true
	}
	bin := strings.TrimSpace(os.Getenv("XKEYSMITH_BIN"))
	if bin == "" {
		return false // 没配二进制:不假装能拉起
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked, s.baseURL = true, baseURL
	if s.cmd != nil && s.cmd.Process != nil {
		// 我们起的进程还活着:再探一次,给它一点启动时间。
		for i := 0; i < 20; i++ {
			if xkHealthy(baseURL) {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}

	cmd := proc.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "XKEYSMITH_SIDECAR=1")
	if err := cmd.Start(); err != nil {
		return false
	}
	s.cmd, s.baseURL = cmd, baseURL
	go func() { _ = cmd.Wait() }() // 回收子进程,避免僵尸
	return xkHealthy(baseURL)
}

// Close 停止我们拉起的进程(网关退出时调用)。
func (s *xkSidecar) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.cmd = nil
}

// xkDefaultPort 是 xkeysmith 的默认监听端口(与桌面端/CLI 一致)。
const xkDefaultPort = "8787"

// xkIsDefaultEndpoint 判断 base URL 是否指向 xkeysmith 的默认端口。
func xkIsDefaultEndpoint(baseURL string) bool {
	rest := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	return strings.HasSuffix(host, ":"+xkDefaultPort)
}

// isLoopbackURL 判断是否本机回环地址。
func isLoopbackURL(baseURL string) bool {
	rest := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host, "]") {
		host = host[:i]
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// xkHealthy 探活:GET /healthz 并核对 service 字段——
// 只认真正的 xkeysmith,不把「端口上有个东西」当成连通。
func xkHealthy(baseURL string) bool {
	root := strings.TrimSuffix(baseURL, "/v1")
	root = strings.TrimSuffix(root, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", root+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	var body struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return body.Service == "xkeysmith"
}

// xkSidecarURL 是配置里 xkeysmith provider 的 Chat base(没有则空)。
func xkSidecarURL(p provider.Provider) string {
	if !xkEnabled(p) {
		return ""
	}
	return p.Base(provider.Chat)
}
