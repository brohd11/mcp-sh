package mcpsh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/mcp-sh/host"
	"github.com/brohd11/mcp-sh/hosttest"
	"github.com/brohd11/mcp-sh/profile"
)

// TestMain doubles as a stdio MCP server, so configs can start this test binary as a
// real upstream subprocess.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_SH_FAKE_MCP") == "1" {
		serveFakeMCP()
		return
	}
	os.Exit(m.Run())
}

type execArgs struct {
	Code       string `json:"code"`
	UserPrompt string `json:"user_prompt"`
}

func serveFakeMCP() {
	fmt.Fprintln(os.Stderr, "fake mcp starting") // lands in the stderr tail
	if os.Getenv("FAKE_MCP_CRASH") == "1" {
		fmt.Fprintln(os.Stderr, "fatal: could not reach Blender on port 9876")
		os.Exit(1)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-blender", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "get_scene_info", Description: "Scene as JSON."},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			UserPrompt string `json:"user_prompt"`
		}) (*mcp.CallToolResult, any, error) {
			text := `{"name":"Scene","objects":[{"name":"Cube","type":"MESH"},{"name":"Light","type":"LIGHT"}],"pid":` + fmt.Sprint(os.Getpid()) + `}`
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "execute_blender_code", Description: "Run Python."},
		func(ctx context.Context, req *mcp.CallToolRequest, in execArgs) (*mcp.CallToolResult, any, error) {
			// Pretend to run the prelude: report ARGS back.
			first, _, _ := strings.Cut(in.Code, "\n")
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Code executed successfully: " + first}}}, nil, nil
		})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

type env struct {
	t    *testing.T
	cfg  Config
	opts profile.Options
}

func newEnv(t *testing.T) *env {
	t.Setenv("MCP_SH_TIMEOUT", "")
	o := profile.Options{ConfigDir: t.TempDir(), ProjectDir: t.TempDir()}
	return &env{t: t, opts: o, cfg: Config{
		Name: "test-shell", App: "test", Version: "v1.2.3", Options: o,
		Builtins: []Builtin{{
			Name: "shout", Summary: "Go-side extra", Source: "builtin",
			Run: func(ctx context.Context, inv *Invocation) int {
				b, _ := io.ReadAll(inv.Stdin)
				io.WriteString(inv.Stdout, strings.ToUpper(string(b)))
				return 0
			},
		}},
	}}
}

// blenderLike is a built-in config in the shape of the Blender app's.
const blenderLike = `{
	"title": "the running Blender session",
	"setup": "1. Install uv",
	"mcpServers": {"blender-mcp": {"command": "uvx", "args": ["blender-mcp"],
		"exec": {"tool": "execute_blender_code", "param": "code", "lang": "python",
		         "outputPrefix": "Code executed successfully: ", "errorPrefix": "Error executing code: "}}}}`

// builtin sets the embedded layer: config.json plus extra files (e.g. commands/x.py).
func (e *env) builtin(config string, files ...string) {
	fsys := fstest.MapFS{"config.json": {Data: []byte(config)}}
	for i := 0; i+1 < len(files); i += 2 {
		fsys[files[i]] = &fstest.MapFile{Data: []byte(files[i+1])}
	}
	e.cfg.FS = fsys
}

func (e *env) writeUserConfig(data string) {
	e.t.Helper()
	path := profile.UserConfigPath(e.cfg.App, e.opts)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) run(stdin string, args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	code := run(e.cfg, args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

func startHost(t *testing.T) string {
	h := &hosttest.Host{
		Name: "testhost", Version: "9", Token: "tok",
		Commands: []host.CommandInfo{{Name: "ping", Summary: "answer pong"}},
		Handlers: map[string]hosttest.Handler{
			"ping": func(host.Request) host.InvokeResult { return host.InvokeResult{Stdout: "pong"} },
		},
	}
	addr, err := h.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return addr
}

// fakeMCPEntry is an mcpServers entry that starts this test binary as a server.
func fakeMCPEntry(t *testing.T, extraEnv string) string {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(exe)
	return fmt.Sprintf(`{"command": %s, "env": {"MCP_SH_FAKE_MCP": "1"%s},
		"defaults": {"user_prompt": ""},
		"exec": {"tool": "execute_blender_code", "param": "code", "lang": "python",
		         "outputPrefix": "Code executed successfully: ", "errorPrefix": "Error executing code: "}}`, b, extraEnv)
}

func TestNativeHost(t *testing.T) {
	e := newEnv(t)
	addr := startHost(t)
	_, port, _ := net.SplitHostPort(addr)
	t.Setenv("TEST_HOST_PORT", port)
	e.writeUserConfig(`{"title": "Test App", "host": {"address": "127.0.0.1:${TEST_HOST_PORT}", "token": "tok"}}`)

	out, errOut, code := e.run("", "run", "ping | shout; exit 4")
	if out != "PONG\n" || code != 4 {
		t.Fatalf("run: %q %q %d", out, errOut, code)
	}
	out, _, code = e.run("ping | wc -c", "run", "-")
	if out != "5\n" || code != 0 {
		t.Fatalf("stdin script: %q %d", out, code)
	}
	out, _, code = e.run("", "commands")
	if code != 0 || !strings.HasPrefix(out, "Sources:\n  testhost 9 (127.0.0.1:") || !strings.Contains(out, "  ping  answer pong") || !strings.Contains(out, "shout") {
		t.Fatalf("commands: %q", out)
	}
}

func TestStdioUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("re-executes the test binary")
	}
	e := newEnv(t)
	e.builtin(blenderLike, "commands/objects.py", "# summary: list scene objects\nprint(ARGS)\n")
	e.writeUserConfig(`{"mcpServers": {"blender-mcp": ` + fakeMCPEntry(t, "") + `}}`)
	cmdDir := filepath.Join(filepath.Dir(profile.UserConfigPath(e.cfg.App, e.opts)), "commands")
	os.MkdirAll(cmdDir, 0o755)
	os.WriteFile(filepath.Join(cmdDir, "mine.py"), []byte("# summary: my command\nprint(ARGS)\n"), 0o644)

	script := `blender-mcp get_scene_info | jq -r '.objects[] | select(.type == "MESH") | .name'
mine a 'b c'
objects | head -1
pid1=$(blender-mcp get_scene_info | jq .pid); pid2=$(blender-mcp get_scene_info | jq .pid)
[ "$pid1" = "$pid2" ] && echo "one upstream process per run"`
	out, errOut, code := e.run("", "run", script)
	want := "Cube\n" + `ARGS = ["a","b c"]; STDIN = ""; CWD = "/"` + "\n" + `ARGS = []; STDIN = ""; CWD = "/"` + "\none upstream process per run\n"
	if out != want || code != 0 {
		t.Fatalf("got %q (stderr %q, exit %d)\nwant %q", out, errOut, code, want)
	}

	out, _, _ = e.run("", "commands")
	for _, s := range []string{"Sources:\n  blender-mcp (", ": 1 commands", "script commands (3 folders): 2 commands", "MCP server, 2 tools: execute_blender_code, get_scene_info", "  mine     my command", "  objects  list scene objects"} {
		if !strings.Contains(out, s) {
			t.Errorf("commands: missing %q in\n%s", s, out)
		}
	}
}

func TestUpstreamCrashShowsStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("re-executes the test binary")
	}
	e := newEnv(t)
	e.writeUserConfig(`{"mcpServers": {"b": ` + fakeMCPEntry(t, `, "FAKE_MCP_CRASH": "1"`) + `}}`)
	_, errOut, code := e.run("", "run", "b get_scene_info")
	if code == 0 || !strings.Contains(errOut, "could not reach Blender on port 9876") {
		t.Fatalf("got %q %d", errOut, code)
	}
}

func TestConfig(t *testing.T) {
	e := newEnv(t)
	e.builtin(`{"title": "Godot", "host": {"address": "127.0.0.1:9510", "token": "${EDITOR_CONSOLE_TOKEN}"}}`)
	t.Setenv("EDITOR_CONSOLE_TOKEN", "hunter2")
	out, _, code := e.run("", "config", "show")
	if code != 0 || strings.Contains(out, "hunter2") || !strings.Contains(out, `"token": "${EDITOR_CONSOLE_TOKEN}"`) {
		t.Fatalf("config show: %q", out)
	}
	out, _, _ = e.run("", "config", "path")
	if !strings.Contains(out, "built-in  found") || !strings.Contains(out, "user      missing") {
		t.Fatalf("config path: %q", out)
	}
}

func TestMCPManagement(t *testing.T) {
	e := newEnv(t)
	e.builtin(blenderLike)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers": {"poly": {"type": "stdio", "command": "npx", "args": ["-y", "poly-mcp"]}}}`), 0o644)

	if _, errOut, code := e.run("", "mcp", "add", "assets", "--env", "K=V", "--", "uvx", "asset-mcp"); code != 0 {
		t.Fatalf("add: %q", errOut)
	}
	if _, errOut, code := e.run("", "mcp", "add", "remote", "--url", "http://x/mcp", "--header", "Authorization: Bearer t"); code != 0 {
		t.Fatalf("add url: %q", errOut)
	}
	if _, errOut, code := e.run("", "mcp", "import", "poly", "--project"); code != 0 {
		t.Fatalf("import: %q", errOut)
	}
	out, _, _ := e.run("", "mcp", "list")
	for _, s := range []string{"assets           uvx asset-mcp", "blender-mcp      uvx blender-mcp  [script commands: python via execute_blender_code]", "poly             npx -y poly-mcp", "remote           http://x/mcp"} {
		if !strings.Contains(out, s) {
			t.Errorf("list: missing %q in\n%s", s, out)
		}
	}
	if !strings.Contains(readFile(t, profile.ProjectConfigPath(e.cfg.App, e.opts)), "poly-mcp") {
		t.Error("import --project did not write the project config")
	}
	if _, _, code := e.run("", "mcp", "remove", "assets"); code != 0 {
		t.Fatal("remove failed")
	}
	_, errOut, code := e.run("", "mcp", "remove", "blender-mcp")
	if code != 1 || !strings.Contains(errOut, `"disabled": true`) {
		t.Fatalf("remove built-in: %q %d", errOut, code)
	}
	if _, errOut, code := e.run("", "mcp", "add", "bad"); code != 2 || !strings.Contains(errOut, "either --url") {
		t.Fatalf("add without target: %q", errOut)
	}
}

func readFile(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSetup(t *testing.T) {
	e := newEnv(t)
	e.builtin(blenderLike)
	e.cfg.Registrations = []Registration{{Suffix: "-game", Env: map[string]string{"PORT_X": "9"}, Label: "game"}}

	out, errOut, code := e.run("", "setup")
	if code != 0 {
		t.Fatalf("setup: %q %q", out, errOut)
	}
	if c := readFile(t, profile.UserConfigPath(e.cfg.App, e.opts)); !strings.Contains(c, "test-shell config show") {
		t.Fatalf("user config comment: %q", c)
	}
	for _, s := range []string{
		"Created " + profile.UserConfigPath(e.cfg.App, e.opts),
		"Register the server with:\n  Claude Code:\n    claude mcp add -s user test-shell -- /",
		"  Codex:\n    codex mcp add test-shell -- /",
		"claude mcp add -s user test-shell-game -e PORT_X=9 -- ",
		"codex mcp add test-shell-game --env PORT_X=9 -- ",
		"  # game\n",
		"In the app:\n  1. Install uv",
		"test-shell commands",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("setup output: missing %q in\n%s", s, out)
		}
	}
	// Second run keeps the config; --scope is Claude's, --name names every entry.
	out, _, _ = e.run("", "setup", "--scope", "project", "--name", "blender")
	for _, s := range []string{"Using existing", "claude mcp add -s project blender -- ", "codex mcp add blender -- ", "codex mcp add blender-game --env"} {
		if !strings.Contains(out, s) {
			t.Errorf("setup --scope/--name: missing %q in\n%s", s, out)
		}
	}
	for _, args := range [][]string{{"setup", "blender"}, {"setup", "--print"}} {
		if _, errOut, code := e.run("", args...); code != 2 || !strings.Contains(errOut, "unexpected argument") {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
	}
}

func TestCLIErrors(t *testing.T) {
	e := newEnv(t)
	if _, errOut, code := e.run("", "run", "true"); code != 2 || !strings.Contains(errOut, "no config for test: expected "+profile.UserConfigPath("test", e.opts)) {
		t.Fatalf("no config: %q", errOut)
	}
	e.builtin(`{"host": {"address": "127.0.0.1:1"}}`)
	if out, _, _ := e.run("", "--version"); out != "v1.2.3\n" {
		t.Fatalf("version: %q", out)
	}
	if out, _, _ := e.run("", "--help"); !strings.Contains(out, "Register the server with:\n  Claude Code:\n    claude mcp add -s user test-shell -- ") || !strings.Contains(out, "codex mcp add test-shell -- ") {
		t.Fatalf("help registration: %q", out)
	}
	if out, _, _ := e.run("", "--help"); !strings.Contains(out, "test-shell setup [--scope") || strings.Contains(out, "profile") || strings.Contains(out, "update [--check]") {
		t.Fatalf("help: %q", out)
	}
	if _, errOut, code := e.run("", "bogus"); code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("bogus: %q", errOut)
	}
	t.Setenv("MCP_SH_TIMEOUT", "soon")
	if _, errOut, code := e.run("", "run", "true"); code != 2 || !strings.Contains(errOut, "MCP_SH_TIMEOUT") {
		t.Fatalf("bad timeout: %q", errOut)
	}
}

func TestSubcommands(t *testing.T) {
	e := newEnv(t)
	var got []string
	e.cfg.Subcommands = []Subcommand{{
		Name: "addon", Usage: "addon install [DIR]", Summary: "Install the editor addon",
		Run: func(args []string, stdout, _ io.Writer) int {
			got = args
			fmt.Fprint(stdout, "installed")
			return 3
		},
	}, {Name: "run", Usage: "run X", Summary: "shadowed", Run: func([]string, io.Writer, io.Writer) int { return 9 }}}
	out, _, code := e.run("", "addon", "install", "x")
	if code != 3 || out != "installed" || strings.Join(got, " ") != "install x" {
		t.Fatalf("dispatch: %q %d %q", out, code, got)
	}
	if _, _, code := e.run("", "run", "true"); code == 9 {
		t.Fatal("subcommand shadowed a core verb")
	}
	if out, _, _ := e.run("", "--help"); !strings.Contains(out, "test-shell addon install [DIR]") || !strings.Contains(out, "Install the editor addon") {
		t.Fatalf("help: %q", out)
	}
}

func TestTimeoutEnv(t *testing.T) {
	e := newEnv(t)
	addr := startHost(t)
	e.writeUserConfig(`{"host": {"address": "` + addr + `", "token": "tok"}}`)
	t.Setenv("MCP_SH_TIMEOUT", "1")
	_, errOut, code := e.run("", "run", "while true; do :; done")
	if code != 124 || !strings.Contains(errOut, "timed out after 1s") {
		t.Fatalf("got %q %d", errOut, code)
	}
}

func TestCommandDirsCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("re-executes the test binary")
	}
	e := newEnv(t)
	e.builtin(blenderLike, "commands/objects.py", "# summary: list scene objects\nprint(ARGS)\n")
	e.writeUserConfig(`{"mcpServers": {"blender-mcp": ` + fakeMCPEntry(t, "") + `}}`)
	tools := t.TempDir()
	os.WriteFile(filepath.Join(tools, "mine.py"), []byte("# summary: from my repo\nprint(1)\n"), 0o644)
	os.WriteFile(filepath.Join(tools, "objects.py"), []byte("# summary: my objects\nprint(2)\n"), 0o644)

	out, errOut, code := e.run("", "commands", "add", tools)
	if code != 0 || !strings.Contains(out, "Added "+tools+" to commandDirs in "+profile.UserConfigPath("test", e.opts)) || errOut != "" {
		t.Fatalf("add: %q %q %d", out, errOut, code)
	}
	if _, errOut, code := e.run("", "commands", "add", tools); code != 1 || !strings.Contains(errOut, "already") {
		t.Fatalf("add twice: %q", errOut)
	}
	out, _, _ = e.run("", "commands")
	for _, s := range []string{"script commands (4 folders): 2 commands", "  mine     from my repo", "  objects  my objects (overrides the one in built-in test commands)"} {
		if !strings.Contains(out, s) {
			t.Errorf("commands: missing %q in\n%s", s, out)
		}
	}
	out, _, _ = e.run("", "config", "path")
	if !strings.Contains(out, "found   "+tools) {
		t.Errorf("config path: %q", out)
	}

	// A folder that does not exist yet is added with a warning, and listed as missing.
	gone := filepath.Join(t.TempDir(), "later")
	if _, errOut, code := e.run("", "commands", "add", gone, "--project"); code != 0 || !strings.Contains(errOut, "not a folder yet") {
		t.Fatalf("add missing: %q %d", errOut, code)
	}
	if out, _, _ := e.run("", "commands"); !strings.Contains(out, "missing: "+gone) {
		t.Errorf("missing folder not reported:\n%s", out)
	}

	if _, errOut, code := e.run("", "commands", "remove", tools); code != 0 {
		t.Fatalf("remove: %q", errOut)
	}
	if _, errOut, code := e.run("", "commands", "remove", tools); code != 1 || !strings.Contains(errOut, "no commandDirs") {
		t.Fatalf("remove twice: %q", errOut)
	}
	if _, errOut, code := e.run("", "commands", "frob", tools); code != 2 || !strings.Contains(errOut, "usage") {
		t.Fatalf("bad subcommand: %q", errOut)
	}
}
