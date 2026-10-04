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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/engine/shengine"
	"github.com/brohd11/agent-shell/host"
	"github.com/brohd11/agent-shell/host/scripts"
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
