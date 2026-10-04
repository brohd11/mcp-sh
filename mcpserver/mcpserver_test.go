package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/engine/shengine"
	"github.com/brohd11/agent-shell/host"
	"github.com/brohd11/agent-shell/hosttest"
	"github.com/brohd11/agent-shell/mcpserver"
	"github.com/brohd11/agent-shell/shell"
)

func connect(t *testing.T, client *host.Client) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	sh := &shell.Shell{Sources: []host.Source{client}, Engine: shengine.New(shengine.Options{})}
	server := mcpserver.New(sh, mcpserver.Options{
		Name: "agent-shell-test", Version: "v0", Title: "the test host",
		Instructions: "Host-specific tip.",
	})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func TestTools(t *testing.T) {
	h := &hosttest.Host{
		Name: "testhost", Version: "1.2",
		Commands: []host.CommandInfo{{Name: "greet", Summary: "say hello", Help: "usage: greet NAME"}},
		Handlers: map[string]hosttest.Handler{
			"greet": func(req host.Request) host.InvokeResult {
				return host.InvokeResult{Stdout: "hello " + strings.Join(req.Args, " ")}
			},
		},
	}
	addr, err := h.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := connect(t, &host.Client{Addr: addr})

	if inst := s.InitializeResult().Instructions; !strings.Contains(inst, "the test host") || !strings.HasSuffix(inst, "Host-specific tip.") {
		t.Errorf("instructions: %q", inst)
	}
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "help,list_commands,run" {
		t.Errorf("tools: %v", names)
	}

	if text, isErr := call(t, s, "run", map[string]any{"script": "greet world | tr a-z A-Z"}); text != "HELLO WORLD" || isErr {
		t.Errorf("run: %q isError=%v", text, isErr)
	}
	if text, isErr := call(t, s, "run", map[string]any{"script": "nope"}); !isErr || !strings.Contains(text, "command not found") || !strings.Contains(text, "[exit_code=127]") {
		t.Errorf("run unknown: %q isError=%v", text, isErr)
	}
	if text, _ := call(t, s, "list_commands", nil); !strings.HasPrefix(text, "Sources:\n  testhost 1.2 (127.0.0.1:") || !strings.Contains(text, "Host commands:\n  greet  say hello") {
		t.Errorf("list_commands: %q", text)
	}
	if text, _ := call(t, s, "help", map[string]any{"command": "greet"}); text != "usage: greet NAME" {
		t.Errorf("help: %q", text)
	}
	if _, isErr := call(t, s, "help", map[string]any{"command": "missing"}); !isErr {
		t.Error("help missing: want isError")
	}
}

func TestHostDown(t *testing.T) {
	s := connect(t, &host.Client{Addr: "127.0.0.1:1", Hint: "start the bridge"})
	text, isErr := call(t, s, "run", map[string]any{"script": "echo hi"})
	if !isErr || !strings.Contains(text, "start the bridge") {
		t.Errorf("run: %q isError=%v", text, isErr)
	}
	if text, isErr := call(t, s, "list_commands", nil); !isErr || !strings.Contains(text, "could not connect") {
		t.Errorf("list_commands: %q isError=%v", text, isErr)
	}
}
