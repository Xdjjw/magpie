package tools

import (
	"encoding/json"
	"testing"
)

type scopeMcpClient struct{ calls []string }

func (c *scopeMcpClient) ListTools() ([]McpTool, error) {
	return []McpTool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}, {Name: "write_note", InputSchema: map[string]any{"type": "object"}}}, nil
}
func (c *scopeMcpClient) CallTool(name string, _ json.RawMessage) (string, bool, error) {
	c.calls = append(c.calls, name)
	return "fixture result", false, nil
}
func (c *scopeMcpClient) Close() {}

func TestMcpExecutionEnforcesServerWhitelistAndGating(t *testing.T) {
	client := &scopeMcpClient{}
	exec := &McpExecutor{Server: McpServerCfg{Name: "fixture", Enabled: true, AllowTools: []string{"echo"}}, Client: client}
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	plane := &Plane{cfg: cfg, guard: Guard{Approval: ApprovalAllow}, executors: []Executor{exec}, defs: exec.Defs(), gating: []string{"xk__mcp__fixture__echo"}}
	for _, name := range []string{"write_note", "echo"} {
		result := plane.Execute(ToolCall{ID: name, Name: "xk__mcp__fixture__" + name, Arguments: map[string]any{}})
		if !result.IsError || len(client.calls) != 0 {
			t.Fatalf("unlisted or locked tool executed: name=%s result=%+v calls=%v", name, result, client.calls)
		}
	}
	if _, _, err := exec.Call("xk__mcp__fixture__write_note", map[string]any{}); err == nil || len(client.calls) != 0 {
		t.Fatalf("direct executor bypassed whitelist: err=%v calls=%v", err, client.calls)
	}
}
