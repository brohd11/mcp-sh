package scripts_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/itchyny/gojq"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/engine/shengine"
	"github.com/brohd11/agent-shell/host"
	"github.com/brohd11/agent-shell/host/scripts"
	"github.com/brohd11/agent-shell/hosttest"
	"github.com/brohd11/agent-shell/shell"
)

// fakeExec mimics blender-mcp's execute_blender_code: success is prefixed text, and
// errors come back as normal text with another prefix.
type fakeExec struct {
	name  string
	codes []string
}

func (f *fakeExec) Name() string { return f.name }

func (f *fakeExec) CallTool(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	code, _ := args["code"].(string)
	f.codes = append(f.codes, code)
	text := "Code executed successfully: ran " + tool
	if strings.Contains(code, "raise") {
		text = "Error executing code: RuntimeError: boom"
	}
	if strings.Contains(code, "PRINT_ARGS") {
		// Echo the prelude back so the test can check what was injected.
		text = "Code executed successfully: " + strings.SplitN(code, "\n# PRINT_ARGS", 2)[0]
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
}

func newShell(t *testing.T, src *scripts.Source) *shell.Shell {
	return &shell.Shell{Sources: []host.Source{src}, Engine: shengine.New(shengine.Options{})}
}

func TestScriptCommands(t *testing.T) {
	builtin := fstest.MapFS{
		"objects.py": {Data: []byte("# summary: list objects\n# Prints one JSON object per line.\n# server: blender\nprint(1)\n")},
		"pick.py":    {Data: []byte("# summary: built-in pick\nprint(2)\n")},
		"notes.txt":  {Data: []byte("ignored: no binding for .txt")},
	}
	userDir := t.TempDir()
	os.WriteFile(filepath.Join(userDir, "pick.py"), []byte("# summary: user pick\n# PRINT_ARGS\n"), 0o644)
	os.WriteFile(filepath.Join(userDir, "boom.py"), []byte("raise RuntimeError('boom')\n"), 0o644)

	exec := &fakeExec{name: "blender"}
	src := &scripts.Source{
		Dirs: []scripts.Dir{{Label: "built-in", FS: builtin}, {Label: "user", FS: os.DirFS(userDir)}},
		Bindings: []scripts.Binding{{
			Server: exec, Tool: "execute_blender_code", Param: "code", Lang: "python",
			OutputPrefix: "Code executed successfully: ", ErrorPrefix: "Error executing code: ",
		}},
	}
	sh := newShell(t, src)

	res := sh.Run(context.Background(), `help | grep -E '^  (objects|pick|boom|notes)'`, 0)
	want := "  boom     script command\n  objects  list objects\n  pick     user pick (overrides the one in built-in)\n"
	if res.Stdout != want {
		t.Fatalf("list: got %q want %q", res.Stdout, want)
	}

	res = sh.Run(context.Background(), `help pick`, 0)
	if !strings.Contains(res.Stdout, "(script pick.py in user; overrides the one in built-in; runs via blender execute_blender_code)") {
		t.Fatalf("override help: %q", res.Stdout)
	}

	res = sh.Run(context.Background(), `help objects`, 0)
	if !strings.Contains(res.Stdout, "Prints one JSON object per line.") || !strings.Contains(res.Stdout, "runs via blender execute_blender_code") || strings.Contains(res.Stdout, "server:") {
		t.Fatalf("help: %q", res.Stdout)
	}

	// User dir overrides built-in; args and stdin are injected; output prefix stripped.
	res = sh.Run(context.Background(), `printf 'a\n"b"\n' | pick "x y" 'it'"'"'s'`, 0)
	wantOut := `ARGS = ["x y","it's"]; STDIN = "a\n\"b\"\n"; CWD = "/"` + "\n# summary: user pick\n"
	if res.Stdout != wantOut || res.ExitCode != 0 {
		t.Fatalf("pick: got %q (stderr %q) want %q", res.Stdout, res.Stderr, wantOut)
	}

	res = sh.Run(context.Background(), `boom || echo "status=$?"`, 0)
	if res.Stdout != "status=1\n" || res.Stderr != "RuntimeError: boom\n" {
		t.Fatalf("boom: %+v", res)
	}

	// Edits apply on the next run.
	os.WriteFile(filepath.Join(userDir, "pick.py"), []byte("# summary: edited\nprint(3)\n"), 0o644)
	res = sh.Run(context.Background(), `help | grep '^  pick'; pick`, 0)
	if res.Stdout != "  pick     edited (overrides the one in built-in)\nran execute_blender_code\n" {
		t.Fatalf("edited: %q", res.Stdout)
	}
	if last := exec.codes[len(exec.codes)-1]; !strings.HasSuffix(last, "print(3)\n") {
		t.Fatalf("stale body sent: %q", last)
	}
}

func TestMissingFolders(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	src := &scripts.Source{Dirs: []scripts.Dir{
		{Label: "default", FS: os.DirFS(filepath.Join(t.TempDir(), "not-created"))},
		{Label: gone, FS: os.DirFS(gone), Required: true},
		{Label: "present", FS: fstest.MapFS{"a.py": {Data: []byte("print(1)\n")}}, Required: true},
	}}
	if got := src.Label(); got != "script commands (3 folders, missing: "+gone+")" {
		t.Fatalf("label: %q", got)
	}
	src.Dirs = src.Dirs[2:]
	if got := src.Label(); got != "script commands (1 folders)" {
		t.Fatalf("label: %q", got)
	}
}

func TestAmbiguousAndMissingServer(t *testing.T) {
	dir := fstest.MapFS{
		"a.py": {Data: []byte("print(1)\n")},
		"b.py": {Data: []byte("# server: nope\nprint(1)\n")},
		"c.py": {Data: []byte("# server: two\nprint(1)\n")},
	}
	src := &scripts.Source{
		Dirs: []scripts.Dir{{Label: "d", FS: dir}},
		Bindings: []scripts.Binding{
			{Server: &fakeExec{name: "one"}, Tool: "t", Param: "code", Lang: "python"},
			{Server: &fakeExec{name: "two"}, Tool: "t", Param: "code", Lang: "python"},
		},
	}
	sh := newShell(t, src)
	res := sh.Run(context.Background(), `a; b; c`, 0)
	if !strings.Contains(res.Stderr, "a: several servers run .py scripts (one, two)") ||
		!strings.Contains(res.Stderr, `b: no server named "nope"`) ||
		res.Stdout != "Code executed successfully: ran t\n" {
		t.Fatalf("got %+v", res)
	}
}

func TestLuauPrelude(t *testing.T) {
	exec := &fakeExec{name: "studio"}
	src := &scripts.Source{
		Dirs:     []scripts.Dir{{Label: "d", FS: fstest.MapFS{"hi.luau": {Data: []byte("-- summary: say hi\nprint('hi')\n")}}}},
		Bindings: []scripts.Binding{{Server: exec, Tool: "run_code", Param: "code", Lang: "luau"}},
	}
	sh := newShell(t, src)
	res := sh.Run(context.Background(), `printf 'tab\there\x01' | hi 'q"uote' 'back\slash' 'é'`, 0)
	if res.ExitCode != 0 {
		t.Fatalf("got %+v", res)
	}
	got := exec.codes[0]
	for _, want := range []string{
		`local ARGS = {"q\"uote", "back\\slash", "é"}; local STDIN = "tab\there\001"; local CWD = "/"; `,
		"local function print(...)",
		"local __agent_shell_main = function() -- summary: say hi\nprint('hi')\n\nend\ndo\n\nend\nlocal __agent_shell_ret = __agent_shell_main()",
		"return table.concat(__agent_shell_out, \"\\n\")",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if h := sh.Run(context.Background(), `help | grep '^  hi'`, 0); !strings.Contains(h.Stdout, "say hi") {
		t.Fatalf("luau header: %q", h.Stdout)
	}
}

func TestPythonPreludeIsValidJSONStrings(t *testing.T) {
	// The prelude's literals are JSON; make sure awkward input round-trips.
	exec := &fakeExec{name: "b"}
	src := &scripts.Source{
		Dirs:     []scripts.Dir{{Label: "d", FS: fstest.MapFS{"p.py": {Data: []byte("pass\n")}}}},
		Bindings: []scripts.Binding{{Server: exec, Tool: "t", Param: "code", Lang: "python"}},
	}
	newShell(t, src).Run(context.Background(), `p '<&>' $' '`, 0)
	line := strings.SplitN(exec.codes[0], "\n", 2)[0]
	var args []string
	argsJSON, _, _ := strings.Cut(strings.TrimPrefix(line, "ARGS = "), "; STDIN = ")
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args[0] != "<&>" || args[1] != " " {
		t.Fatalf("args line %q -> %q %v", line, args, err)
	}
}

func TestLibrariesAndErrorTrim(t *testing.T) {
	exec := &errExec{name: "studio"}
	src := &scripts.Source{
		Dirs: []scripts.Dir{
			{Label: "built-in", FS: fstest.MapFS{
				"_lib.luau": {Data: []byte("function lib.a() end")},
				"go.luau":   {Data: []byte("-- summary: go\nlib.a()\n")},
			}},
			{Label: "user", FS: fstest.MapFS{"_lib.luau": {Data: []byte("function lib.b() end")}}},
		},
		Bindings: []scripts.Binding{{
			Server: exec, Tool: "execute_luau", Param: "code", Lang: "luau",
			ErrorTrim: regexp.MustCompile(`(?:sabuiltin_\S+:\d+: )+`),
		}},
	}
	sh := newShell(t, src)
	res := sh.Run(context.Background(), `help | grep -c '^  _lib'; go`, 0)
	if res.Stdout != "0\n" {
		t.Fatalf("_lib listed as a command: %q", res.Stdout)
	}
	if res.Stderr != "AssistantCommand:2: boom\n" || res.ExitCode != 1 {
		t.Fatalf("error trim: %+v", res)
	}
	code := exec.codes[0]
	a, b, body := strings.Index(code, "function lib.a()"), strings.Index(code, "function lib.b()"), strings.Index(code, "lib.a()\n")
	if a < 0 || b < a || body > a {
		t.Fatalf("library order or placement wrong:\n%s", code)
	}
}

// errExec fails like Studio's execute_luau does.
type errExec struct {
	name  string
	codes []string
}

func (e *errExec) Name() string { return e.name }

func (e *errExec) CallTool(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	e.codes = append(e.codes, args["code"].(string))
	text := "sabuiltin_Assistant.rbxm.Tools.ExecuteLuauTool:66: sabuiltin_Assistant.rbxm.Util.CommandExecution:54: AssistantCommand:2: boom"
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
}

// callAPIExec mimics gimp-mcp's call_api: the code is nested in args, output is a JSON
// list of printed text, and errors are "Error: " plus a JSON-quoted message (or plain
// text when GIMP is unreachable).
type callAPIExec struct {
	inputs []map[string]any
}

func (c *callAPIExec) Name() string { return "gimp" }

func (c *callAPIExec) CallTool(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	c.inputs = append(c.inputs, args)
	code := args["args"].([]any)[1].([]any)[0].(string)
	text := `["hello\n", "world"]`
	switch {
	case strings.Contains(code, "raise"):
		text = `Error: "name 'x' is not defined"`
	case strings.Contains(code, "offline"):
		text = "Error: Could not connect to GIMP at localhost:9877."
	case strings.Contains(code, "plain"):
		text = "not json"
	case strings.Contains(code, "scalar"):
		text = `"not a list"`
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
}

func TestArgsTemplateAndJQ(t *testing.T) {
	exec := &callAPIExec{}
	jq := func(src string) *gojq.Code {
		q, err := gojq.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		code, err := gojq.Compile(q)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	template := map[string]any{"api_path": "exec", "args": []any{"pyGObject-console", []any{scripts.CodeMarker}}}
	src := &scripts.Source{
		Dirs: []scripts.Dir{{Label: "d", FS: fstest.MapFS{
			"ok.py":      {Data: []byte("print('hello')\n")},
			"bad.py":     {Data: []byte("raise x\n")},
			"offline.py": {Data: []byte("# offline\n")},
			"plain.py":   {Data: []byte("# plain\n")},
			"scalar.py":  {Data: []byte("# scalar\n")},
		}}},
		Bindings: []scripts.Binding{{
			Server: exec, Tool: "call_api", Args: template, Lang: "python",
			ErrorPrefix: "Error: ", OutputJQ: jq(`join("")`), ErrorJQ: jq(`.`),
		}},
	}
	sh := newShell(t, src)

	res := sh.Run(context.Background(), `ok`, 0)
	if res.Stdout != "hello\nworld\n" || res.ExitCode != 0 {
		t.Fatalf("ok: %+v", res)
	}
	in := exec.inputs[0]
	nested := in["args"].([]any)
	if in["api_path"] != "exec" || nested[0] != "pyGObject-console" || !strings.HasSuffix(nested[1].([]any)[0].(string), "print('hello')\n") {
		t.Fatalf("input: %#v", in)
	}
	if template["args"].([]any)[1].([]any)[0] != scripts.CodeMarker {
		t.Fatalf("template was modified: %#v", template)
	}

	cases := []struct{ script, stdout, stderr string }{
		{`bad`, "", "name 'x' is not defined\n"},
		{`offline`, "", "Could not connect to GIMP at localhost:9877.\n"},
		{`plain`, "not json\n", ""},
		{`scalar || echo "status=$?"`, "status=1\n", "scalar: output filter: join(\"\") cannot be applied to: string (\"not a list\")\n"},
	}
	for _, c := range cases {
		res := sh.Run(context.Background(), c.script, 0)
		if res.Stdout != c.stdout || res.Stderr != c.stderr {
			t.Errorf("%s: got stdout %q stderr %q, want %q %q", c.script, res.Stdout, res.Stderr, c.stdout, c.stderr)
		}
	}
}

func TestHostBinding(t *testing.T) {
	fake := &hosttest.Host{
		Name:     "gimp",
		Commands: []host.CommandInfo{{Name: "python", Summary: "run Python from stdin"}},
		Handlers: map[string]hosttest.Handler{
			"python": func(req host.Request) host.InvokeResult {
				if strings.Contains(req.Stdin, "raise") {
					return host.InvokeResult{Stdout: "printed first", Stderr: "RuntimeError: boom", ExitCode: 1}
				}
				// Echo the prelude, so the test sees what was injected.
				return host.InvokeResult{Stdout: strings.SplitN(req.Stdin, "\n", 2)[0]}
			},
		},
	}
	addr, err := fake.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	client := &host.Client{Addr: addr}
	src := &scripts.Source{
		Dirs: []scripts.Dir{{Label: "d", FS: fstest.MapFS{
			"show.py": {Data: []byte("# summary: show\nprint(ARGS)\n")},
			"bad.py":  {Data: []byte("# server: host\nprint('printed first')\nraise RuntimeError('boom')\n")},
		}}},
		Bindings: []scripts.Binding{{Host: client, Tool: "python", Lang: "python"}},
	}
	sh := &shell.Shell{Sources: []host.Source{client, src}, Engine: shengine.New(shengine.Options{})}

	res := sh.Run(context.Background(), `show 'a b' c`, 0)
	if res.Stdout != `ARGS = ["a b","c"]; STDIN = ""; CWD = "/"`+"\n" || res.ExitCode != 0 {
		t.Fatalf("show: %+v", res)
	}
	// Output printed before an error is kept, and the host's exit code passes through.
	res = sh.Run(context.Background(), `bad; echo "status=$?"`, 0)
	if res.Stdout != "printed first\nstatus=1\n" || res.Stderr != "RuntimeError: boom\n" {
		t.Fatalf("bad: %+v", res)
	}
	res = sh.Run(context.Background(), `help bad | tail -1; help | grep -c '^  python'`, 0)
	if !strings.Contains(res.Stdout, "runs via host python") || !strings.HasSuffix(res.Stdout, "1\n") {
		t.Fatalf("help: %q", res.Stdout)
	}
	if inv := fake.Invokes(); inv[0].Cmd != "python" || len(inv[0].Args) != 0 {
		t.Fatalf("invoke: %+v", inv[0])
	}
}
