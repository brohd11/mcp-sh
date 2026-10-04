package mcphost_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/engine/shengine"
	"github.com/brohd11/agent-shell/host"
	"github.com/brohd11/agent-shell/host/mcphost"
	"github.com/brohd11/agent-shell/shell"
)

type moveIn struct {
	Name     string   `json:"name" jsonschema:"object to move"`
	X        float64  `json:"x"`
	Y        float64  `json:"y"`
	Relative bool     `json:"relative,omitempty"`
	Count    int      `json:"count,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Meta     any      `json:"meta,omitempty"`
}

type getIn struct {
	ObjectName string `json:"object_name" jsonschema:"name of the object"`
	UserPrompt string `json:"user_prompt"`
}

type execIn struct {
	Code string `json:"code"`
}

// fakeUpstream is an MCP server reachable over fresh in-memory transports, so tests
// can kill its sessions and watch the client reconnect.
type fakeUpstream struct {
	server *mcp.Server
	mu     sync.Mutex
	conns  []*mcp.ServerSession
	down   bool
}

func newFakeUpstream() *fakeUpstream {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, &mcp.ServerOptions{Instructions: "Fake server instructions."})
	echo := func(v any) (*mcp.CallToolResult, any, error) {
		b, _ := json.Marshal(v)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
	}
	mcp.AddTool(s, &mcp.Tool{Name: "move", Description: "Move an object. Units are meters."},
		func(ctx context.Context, req *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, any, error) {
			var raw map[string]any
			json.Unmarshal(req.Params.Arguments, &raw)
			return echo(raw)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_object", Description: "Get object info."},
		func(ctx context.Context, req *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, any, error) {
			return echo(in)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "fail", Description: "Always fails."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "it broke"}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "soft_fail", Description: "Reports failure as text."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Error getting info: not connected"}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "secret", Description: "Should be hidden."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return echo("secret")
		})
	mcp.AddTool(s, &mcp.Tool{Name: "exec", Description: "Run code."},
		func(ctx context.Context, req *mcp.CallToolRequest, in execIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Code executed successfully: " + in.Code}}}, nil, nil
		})
	s.AddTool(&mcp.Tool{Name: "structured", Description: "Structured only.", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"ok": true, "n": 2}}, nil
		})
	return &fakeUpstream{server: s}
}

func (f *fakeUpstream) transport() (mcp.Transport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errors.New("server is down")
	}
	st, ct := mcp.NewInMemoryTransports()
	ss, err := f.server.Connect(context.Background(), st, nil)
	if err != nil {
		return nil, err
	}
	f.conns = append(f.conns, ss)
	return ct, nil
}

func (f *fakeUpstream) killSessions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
	f.conns = nil
}

func (f *fakeUpstream) connects() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

func newShell(t *testing.T, srv *mcphost.Server) *shell.Shell {
	t.Helper()
	sh := &shell.Shell{Sources: []host.Source{srv}, Engine: shengine.New(shengine.Options{})}
	t.Cleanup(sh.Close)
	return sh
}

func setup(t *testing.T) (*shell.Shell, *fakeUpstream) {
	up := newFakeUpstream()
	srv := mcphost.NewWithTransport(mcphost.Config{
		Name:         "fake",
		HideTools:    []string{"secret"},
		Defaults:     map[string]any{"user_prompt": "from-defaults", "not_a_param": 1},
		ErrorPattern: regexp.MustCompile(`^Error\b`),
	}, up.transport)
	return newShell(t, srv), up
}

func run(t *testing.T, sh *shell.Shell, script string) shell.Result {
	t.Helper()
	return sh.Run(context.Background(), script, 0)
}

func TestArgsCoercion(t *testing.T) {
	sh, _ := setup(t)
	cases := []struct{ script, want string }{
		{`fake move --name Cube --x 1.5 --y=2 --relative --count 3 --tags a --tags b`,
			`{"count":3,"name":"Cube","relative":true,"tags":["a","b"],"x":1.5,"y":2}`},
		{`fake move --name C --x 0 --y 0 --relative false --tags '["p","q"]' --meta '{"k":[1]}'`,
			`{"meta":{"k":[1]},"name":"C","relative":false,"tags":["p","q"],"x":0,"y":0}`},
		{`fake move --json '{"name":"J","x":1,"y":2}' --x 9`, `{"name":"J","x":9,"y":2}`},
		{`echo '{"name":"S","x":1,"y":2}' | fake move --json -`, `{"name":"S","x":1,"y":2}`},
		{`fake move --name N --x 1 --y 1 --no-relative`, `{"name":"N","relative":false,"x":1,"y":1}`},
		// Positional fills the single required param; defaults fill declared params only.
		{`fake get_object --object-name Cube`, `{"object_name":"Cube","user_prompt":"from-defaults"}`},
		{`fake get-object --object_name Cube --user_prompt mine`, `{"object_name":"Cube","user_prompt":"mine"}`},
	}
	for _, c := range cases {
		res := run(t, sh, c.script)
		if strings.TrimSpace(res.Stdout) != c.want || res.ExitCode != 0 {
			t.Errorf("%s\n got %q (exit %d, stderr %q, err %v)\nwant %q", c.script, res.Stdout, res.ExitCode, res.Stderr, res.Err, c.want)
		}
	}
}

func TestArgErrors(t *testing.T) {
	sh, _ := setup(t)
	cases := []struct {
		script, stderr string
		code           int
	}{
		{`fake move --name A`, "missing required --x, --y", 2},
		{`fake move --name A --x abc --y 1`, `--x: invalid value "abc" (expected number)`, 2},
		{`fake move --name A --x 1 --y 1 --bogus 1`, "unknown parameter --bogus", 2},
		{`fake move A`, "pass parameters as --name value", 2},
		{`fake move --json '[1]'`, "expected a JSON object", 2},
		{`fake nope`, `no tool named "nope"`, 127},
		{`fake secret`, `no tool named "secret"`, 127},
		{`fake fail`, "it broke", 1},
		{`fake soft_fail`, "Error getting info: not connected", 1},
	}
	for _, c := range cases {
		res := run(t, sh, c.script)
		if res.ExitCode != c.code || !strings.Contains(res.Stderr, c.stderr) {
			t.Errorf("%s\n got exit %d stderr %q\nwant exit %d containing %q", c.script, res.ExitCode, res.Stderr, c.code, c.stderr)
		}
	}
}

func TestOutputAndPipes(t *testing.T) {
	sh, _ := setup(t)
	res := run(t, sh, `fake structured | jq -c .; fake get_object Cube | jq -r .object_name`)
	if res.Stdout != "{\"n\":2,\"ok\":true}\nCube\n" {
		t.Fatalf("got %q (stderr %q)", res.Stdout, res.Stderr)
	}
}

func TestHelp(t *testing.T) {
	sh, _ := setup(t)
	res := run(t, sh, `help fake`)
	for _, want := range []string{"usage: fake TOOL", "  move ", "Move an object.", "Server instructions:\nFake server instructions."} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("help fake: missing %q in:\n%s", want, res.Stdout)
		}
	}
	if strings.Contains(res.Stdout, "secret") || strings.Contains(res.Stdout, "Units are meters") {
		t.Errorf("help fake: hidden tool or full description leaked:\n%s", res.Stdout)
	}
	res = run(t, sh, `fake move --help`)
	for _, want := range []string{
		"usage: fake move --name STRING --x NUMBER --y NUMBER [options]",
		"Move an object. Units are meters.",
		"--name string", "object to move (required)",
		"--tags array of string",
		"--json - reads it from stdin",
	} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("move --help: missing %q in:\n%s", want, res.Stdout)
		}
	}
	res = run(t, sh, `help | grep '^  fake'`)
	if !strings.Contains(res.Stdout, "MCP server, 6 tools: exec, fail, get_object, move, soft_fail, structured") {
		t.Errorf("list: %q", res.Stdout)
	}
}

func TestReconnect(t *testing.T) {
	sh, up := setup(t)
	if res := run(t, sh, `fake get_object A`); res.ExitCode != 0 {
		t.Fatalf("first: %+v", res)
	}
	if res := run(t, sh, `fake get_object B`); res.ExitCode != 0 || up.connects() != 1 {
		t.Fatalf("session should be reused: %+v, connects %d", res, up.connects())
	}
	up.killSessions()
	res := run(t, sh, `fake get_object C | jq -r .object_name`)
	if res.Stdout != "C\n" || res.ExitCode != 0 {
		t.Fatalf("after kill: %+v", res)
	}
}

func TestUnavailable(t *testing.T) {
	up := newFakeUpstream()
	up.down = true
	srv := mcphost.NewWithTransport(mcphost.Config{Name: "fake"}, up.transport)
	sh := newShell(t, srv)
	res := run(t, sh, `echo hi`)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "server is down") {
		t.Fatalf("only source down: want error, got %+v", res)
	}

	// With a second, working source the dead one is a warning plus a stub command.
	good := mcphost.NewWithTransport(mcphost.Config{Name: "good"}, newFakeUpstream().transport)
	sh2 := &shell.Shell{Sources: []host.Source{srv, good}, Engine: shengine.New(shengine.Options{})}
	defer sh2.Close()
	res = run(t, sh2, `good get_object --object_name X --user_prompt p | jq -r .object_name; fake get_object X`)
	if res.Stdout != "X\n" || res.ExitCode != 1 || !strings.Contains(res.Stderr, "fake: MCP server unavailable: server is down") || len(res.Warnings) != 1 {
		t.Fatalf("got %+v", res)
	}
}

func TestParseArgsSchemaShapes(t *testing.T) {
	// Optional[int] from Python servers: anyOf [{type: integer}, {type: null}].
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"n":    map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}},
			"mode": map[string]any{"type": "string", "enum": []any{"a", "b"}},
			"free": map[string]any{},
		},
	}
	got, err := mcphost.ParseArgs(schema, []string{"--n", "5", "--mode", "a", "--free", `{"x":1}`}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"free":{"x":1},"mode":"a","n":5}` {
		t.Fatalf("got %s", b)
	}
	if _, err := mcphost.ParseArgs(schema, []string{"--n", "null"}, nil, nil); err != nil {
		t.Fatalf("null for optional: %v", err)
	}
	// No properties at all: flags pass through, JSON-ish values decoded.
	got, err = mcphost.ParseArgs(map[string]any{"type": "object"}, []string{"--a", "1", "--b", "text"}, nil, nil)
	if b, _ := json.Marshal(got); err != nil || string(b) != `{"a":1,"b":"text"}` {
		t.Fatalf("passthrough: %s %v", b, err)
	}
}

func TestDynamicDefaults(t *testing.T) {
	var mu sync.Mutex
	studios := []string{"s-1"}
	listCalls := 0
	s := mcp.NewServer(&mcp.Implementation{Name: "studio", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "list_studios"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			mu.Lock()
			defer mu.Unlock()
			listCalls++
			var list []map[string]string
			for _, id := range studios {
				list = append(list, map[string]string{"id": id})
			}
			b, _ := json.Marshal(map[string]any{"studios": list})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
		})
	type stateIn struct {
		StudioID string `json:"studio_id"`
		Mode     string `json:"mode"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "state"},
		func(ctx context.Context, req *mcp.CallToolRequest, in stateIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.StudioID + "/" + in.Mode}}}, nil, nil
		})
	up := &fakeUpstream{server: s}
	jq := `(.studios | length) as $n | if $n == 1 then .studios[0].id else error("\($n) Studios are open: pass --studio_id") end`
	spec := map[string]any{"$tool": "list_studios", "$jq": jq}
	if _, ok, err := mcphost.ParseDynamicDefault(spec); !ok || err != nil {
		t.Fatalf("spec: %v %v", ok, err)
	}
	srv := mcphost.NewWithTransport(mcphost.Config{Name: "studio", Defaults: map[string]any{"studio_id": spec, "mode": "Edit"}}, up.transport)
	sh := newShell(t, srv)

	res := run(t, sh, `studio state; studio state --mode Play; studio state --studio_id other`)
	if res.Stdout != "s-1/Edit\ns-1/Play\nother/Edit\n" || res.ExitCode != 0 {
		t.Fatalf("got %+v", res)
	}
	if listCalls != 1 {
		t.Fatalf("dynamic default resolved %d times in one run, want 1", listCalls)
	}
	help := run(t, sh, `studio state --help`).Stdout
	if !strings.HasPrefix(help, "usage: studio state [options]") || !strings.Contains(help, "config fills it from list_studios") || !strings.Contains(help, `config default "Edit"`) {
		t.Fatalf("help: %s", help)
	}

	mu.Lock()
	studios = []string{"s-1", "s-2"}
	mu.Unlock()
	res = run(t, sh, `studio state`)
	if res.ExitCode != 1 || !strings.Contains(res.Stderr, "--studio_id was not given and its default failed: 2 Studios are open") {
		t.Fatalf("several: %+v", res)
	}
	if res := run(t, sh, `studio state --studio_id s-2`); res.Stdout != "s-2/Edit\n" {
		t.Fatalf("explicit: %+v", res)
	}

	for _, bad := range []any{
		map[string]any{"$tool": ""},
		map[string]any{"$tool": "x", "$jq": ".["},
		map[string]any{"$tool": "x", "$bogus": 1},
	} {
		if _, ok, err := mcphost.ParseDynamicDefault(bad); !ok || err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
	if _, ok, _ := mcphost.ParseDynamicDefault(map[string]any{"plain": "object"}); ok {
		t.Error("plain object treated as dynamic")
	}
}
