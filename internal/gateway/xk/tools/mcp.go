package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// MCP 桥(C 档):工具实现留在 MCP 侧,网关只负责转发。
// 支持 stdio 和 Streamable HTTP 两种传输。

type McpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type McpClient interface {
	ListTools() ([]McpTool, error)
	CallTool(name string, arguments json.RawMessage) (text string, isError bool, err error)
	Close()
}

// ConnectMcp 按配置连接一个 MCP 服务器。
// ConnectMcp connects an MCP server. A cancelled ctx aborts the handshake,
// which is how Plane.Close stops a connection it started and no longer
// wants: without it the goroutine went on connecting to a plane that was
// already closed.
func ConnectMcp(cfg McpServerCfg, timeoutMS uint64) (McpClient, error) {
	return ConnectMcpContext(context.Background(), cfg, timeoutMS)
}

// ConnectMcpContext is ConnectMcp with the caller's context.
func ConnectMcpContext(ctx context.Context, cfg McpServerCfg, timeoutMS uint64) (McpClient, error) {
	switch cfg.Transport {
	case "stdio", "":
		return newStdioClient(ctx, cfg, timeoutMS)
	case "http", "streamable-http":
		return newHTTPClient(ctx, cfg, timeoutMS)
	default:
		return nil, fmt.Errorf("未知 MCP 传输: %s", cfg.Transport)
	}
}

// ─────────────────────────── stdio 客户端 ───────────────────────────

type stdioClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	output  io.ReadCloser
	mu      sync.Mutex
	nextID  int
	timeout time.Duration
}

func newStdioClient(ctx context.Context, cfg McpServerCfg, timeoutMS uint64) (*stdioClient, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("stdio 传输但没有配 command")
	}
	// cwd 不存在时 Command 报的错只说 command 名,排查方向会跑偏——
	// 先单独检查它(尤其当 cwd 是没展开的环境变量时)。
	if cfg.Cwd != "" {
		if info, err := os.Stat(cfg.Cwd); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("cwd `%s` 不是存在的目录(检查它是否写成了未定义的环境变量)", cfg.Cwd)
		}
	}
	cmd := proc.Command(cfg.Command, cfg.Args...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	if len(cfg.Env) > 0 {
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 服务器失败: %w", err)
	}
	c := &stdioClient{
		cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<20), output: stdout,
		timeout: time.Duration(timeoutMS) * time.Millisecond,
	}
	if err := c.initialize(ctx); err != nil {
		c.abort()
		_ = cmd.Wait()
		return nil, err
	}
	return c, nil
}

func (c *stdioClient) send(method string, params any, notify bool) (json.RawMessage, error) {
	return c.sendContext(context.Background(), method, params, notify)
}

func (c *stdioClient) sendContext(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, c.abort)
	defer stop()
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	if !notify {
		c.nextID++
		req["id"] = c.nextID
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	if notify {
		return nil, nil
	}
	// 读一行应答(跳过通知:没有 id 的消息)。
	for {
		respLine, err := c.stdout.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("MCP 连接断开: %w", err)
		}
		var resp struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(respLine, &resp); err != nil {
			continue
		}
		if resp.ID == nil || *resp.ID != c.nextID {
			continue // 通知,不是我们的应答
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP 错误 %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (c *stdioClient) initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.sendContext(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "magpie-xkeysmith", "version": "1"},
	}, false)
	if err != nil {
		return err
	}
	_, err = c.sendContext(ctx, "notifications/initialized", nil, true)
	return err
}

func (c *stdioClient) ListTools() ([]McpTool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := c.send("tools/list", map[string]any{}, false)
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

func (c *stdioClient) CallTool(name string, arguments json.RawMessage) (string, bool, error) {
	return c.CallToolContext(context.Background(), name, arguments)
}

func (c *stdioClient) CallToolContext(ctx context.Context, name string, arguments json.RawMessage) (string, bool, error) {
	if err := lockMCP(ctx, &c.mu); err != nil {
		return "", true, err
	}
	defer c.mu.Unlock()
	var args any
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return "", true, fmt.Errorf("参数不是合法 JSON: %w", err)
		}
	}
	raw, err := c.sendContext(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, false)
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

func (c *stdioClient) Close() {
	c.abort()
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.cmd.Wait()
}

// Cancellation closes the pipes so a blocked ReadBytes ends too. A stdio
// connection interrupted mid-response cannot safely serve another request.
func (c *stdioClient) abort() {
	_ = c.stdin.Close()
	if c.output != nil {
		_ = c.output.Close()
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func lockMCP(ctx context.Context, mu *sync.Mutex) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// EnvExpand 展开 `command`/`args`/`cwd` 里的 ${VAR} 与 $VAR;
// 未定义的变量原样保留(展开成空串会把失败点推得离原因很远)。
func EnvExpand(input string) string {
	var out []byte
	for i := 0; i < len(input); {
		if input[i] != '$' {
			out = append(out, input[i])
			i++
			continue
		}
		if i+1 < len(input) && input[i+1] == '{' {
			end := -1
			for j := i + 2; j < len(input); j++ {
				if input[j] == '}' {
					end = j
					break
				}
			}
			if end < 0 {
				out = append(out, '$')
				i++
				continue
			}
			name := input[i+2 : end]
			if v, ok := os.LookupEnv(name); ok && name != "" {
				out = append(out, v...)
			} else {
				out = append(out, input[i:end+1]...)
			}
			i = end + 1
			continue
		}
		j := i + 1
		for j < len(input) && (isAlnum(input[j]) || input[j] == '_') {
			j++
		}
		if j == i+1 {
			out = append(out, '$')
			i++
			continue
		}
		name := input[i+1 : j]
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, v...)
		} else {
			out = append(out, input[i:j]...)
		}
		i = j
	}
	return string(out)
}

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// AbsCwd 把相对 cwd 锚到给定基准(打包应用不依赖进程 CWD)。
func AbsCwd(cwd, base string) string {
	if cwd == "" || filepath.IsAbs(cwd) {
		return cwd
	}
	return filepath.Join(base, cwd)
}
