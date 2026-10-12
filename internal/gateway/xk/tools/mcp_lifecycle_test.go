package tools

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// Run the test binary as a real stdio server. Its control connection says
// when tools/list starts and closes when the child exits; closing it from
// the parent also lets the child exit if an assertion fails.
func TestMcpStdioDiscoveryHelperProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "stdio-discovery" {
		return
	}
	control, err := net.DialTimeout("tcp", os.Args[len(os.Args)-1], 5*time.Second)
	if err != nil {
		os.Exit(2)
	}
	defer control.Close()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(2)
		}
		switch req.Method {
		case "initialize":
			if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
					"serverInfo": map[string]any{"name": "discovery", "version": "1"},
				},
			}); err != nil {
				os.Exit(2)
			}
		case "notifications/initialized":
		case "tools/list":
			if _, err := io.WriteString(control, "tools/list\n"); err != nil {
				os.Exit(2)
			}
			_, _ = control.Read(make([]byte, 1))
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func TestMcpStdioCloseCancelsToolDiscovery(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultToolsConfig()
	cfg.Enabled = true
	cfg.CallTimeoutMS = 30_000
	p := BuildAsync(cfg, skillsFixture(), []McpServerCfg{{
		Name: "discovery", Enabled: true, Transport: "stdio", Command: command,
		Args: []string{"-test.run=^TestMcpStdioDiscoveryHelperProcess$", "--", "stdio-discovery", listener.Addr().String()},
	}})
	defer p.Close()
	control, err := listener.AcceptTCP()
	if err != nil {
		t.Fatalf("stdio server did not connect: %v; status=%+v", err, p.McpStatus())
	}
	defer control.Close()
	if err := control.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(control)
	if phase, err := reader.ReadString('\n'); err != nil || phase != "tools/list\n" {
		t.Fatalf("stdio discovery did not reach tools/list: phase=%q err=%v", phase, err)
	}
	p.Close()
	if err := control.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("Close left the stdio tools/list process running: %v", err)
	}
	if status := p.McpStatus(); len(status) != 0 {
		t.Fatalf("closed plane published a discovery result: %+v", status)
	}
}
