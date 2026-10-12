package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Streamable HTTP 传输(MCP 2025-06-18)。
//
// 一次 POST 发一条 JSON-RPC;服务器可以两种方式回答:
//   - content-type: application/json —— 直接就是响应体;
//   - content-type: text/event-stream —— 用 SSE 回,可能夹带通知/进度事件,
//     要一直读到 **id 匹配**的那一条为止。
//
// 会话:服务器在 Mcp-Session-Id 响应头里给会话 id,之后每个请求都要带回去。
// 刻意**不**实现 legacy SSE(2024-11-05 的 GET /sse + POST /message):
// 配置里没有对应取值,因此不存在「配了却不生效」。

const sessionHeader = "Mcp-Session-Id"

type httpClient struct {
	server    string
	url       string
	headers   map[string]string
	client    *http.Client
	sessionID string
	nextID    int
	mu        sync.Mutex
	// ctx 是连接的生命周期(平面的 conn):Close 取消它,发现期的每个
	// 请求都从它派生 —— 否则 initialize 之外的阶段要等自身超时才罢手。
	ctx context.Context
}

func newHTTPClient(ctx context.Context, cfg McpServerCfg, timeoutMS uint64) (*httpClient, error) {
	if cfg.Command != "" && cfg.Transport == "http" {
		return nil, fmt.Errorf("http 传输不接受 command")
	}
	if !strings.HasPrefix(cfg.URL, "http") {
		return nil, fmt.Errorf("http 传输但 url `%s` 不是 http(s) 地址", cfg.URL)
	}
	timeout := time.Duration(timeoutMS) * time.Millisecond
	if timeout < time.Second {
		timeout = time.Second
	}
	c := &httpClient{
		server: cfg.Name, url: cfg.URL, headers: cfg.Headers,
		client: &http.Client{Timeout: timeout}, nextID: 1, ctx: ctx,
	}
	if _, err := c.requestContext(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "magpie-xkeysmith", "version": "1"},
	}); err != nil {
		return nil, err
	}
	if err := c.notifyContext(ctx, "notifications/initialized"); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *httpClient) request(method string, params any) (json.RawMessage, error) {
	return c.requestContext(context.Background(), method, params)
}

func (c *httpClient) requestContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := lockMCP(ctx, &c.mu); err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID - 1
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.sessionID != "" {
		req.Header.Set(sessionHeader, c.sessionID)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MCP HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if c.sessionID == "" {
		if sid := resp.Header.Get(sessionHeader); sid != "" {
			c.sessionID = sid
		}
	}
	ct := resp.Header.Get("content-type")
	if strings.Contains(ct, "text/event-stream") {
		return c.readSSE(resp.Body, id)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return extractResult(raw)
}

func (c *httpClient) notify(method string) error {
	return c.notifyContext(c.ctx, method)
}

// notifyContext sends a notification derived from ctx: one sent during
// discovery carries the plane's lifecycle, so Close ends it at once.
func (c *httpClient) notifyContext(ctx context.Context, method string) error {
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.sessionID != "" {
		req.Header.Set(sessionHeader, c.sessionID)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// readSSE 一直读到 id 匹配的那一条(中间的进度/通知事件跳过)。
func (c *httpClient) readSSE(body io.Reader, wantID int) (json.RawMessage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var probe struct {
			ID *int `json:"id"`
		}
		if json.Unmarshal([]byte(data), &probe) != nil {
			continue
		}
		if probe.ID == nil || *probe.ID != wantID {
			continue // 通知或别的请求的应答
		}
		return extractResult([]byte(data))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("MCP SSE 流结束但没等到 id=%d 的应答", wantID)
}

func extractResult(raw []byte) (json.RawMessage, error) {
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("MCP 应答不是合法 JSON: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP 错误 %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

func (c *httpClient) ListTools() ([]McpTool, error) {
	// 工具发现属于连接建立的一部分:从生命周期 ctx 派生,Close 即取消,
	// 不等 CallTimeoutMS 自己到点。
	raw, err := c.requestContext(c.ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []McpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

func (c *httpClient) CallTool(name string, arguments json.RawMessage) (string, bool, error) {
	return c.CallToolContext(context.Background(), name, arguments)
}

func (c *httpClient) CallToolContext(ctx context.Context, name string, arguments json.RawMessage) (string, bool, error) {
	var args any
	if len(arguments) > 0 {
		_ = json.Unmarshal(arguments, &args)
	}
	raw, err := c.requestContext(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", true, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), false, nil
	}
	var text string
	for _, part := range out.Content {
		if part.Type == "text" {
			text += part.Text
		}
	}
	return text, out.IsError, nil
}

func (c *httpClient) Close() {
	// 生命周期 ctx(c.ctx)由平面的 Close 取消,在途请求随之结束;
	// 这里再放掉空闲连接,不留池子。
	c.client.CloseIdleConnections()
}
