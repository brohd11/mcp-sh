package profile_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/brohd11/mcp-sh/host"
	"github.com/brohd11/mcp-sh/host/mcphost"
	"github.com/brohd11/mcp-sh/host/scripts"
	"github.com/brohd11/mcp-sh/profile"
)

func opts(t *testing.T) profile.Options {
	return profile.Options{ConfigDir: t.TempDir(), ProjectDir: t.TempDir()}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// blender is an app in the shape of the Blender app: a built-in config and commands.
var blender = profile.App{Name: "blender", FS: fstest.MapFS{
	"config.json": {Data: []byte(`{
		"title": "the running Blender session",
		"instructions": "use blender-mcp",
		"mcpServers": {"blender-mcp": {
			"command": "uvx", "args": ["blender-mcp"],
			"defaults": {"user_prompt": ""},
			"exec": {"tool": "execute_blender_code", "param": "code", "lang": "python"}
		}}
	}`)},
	"commands/objects.py": {Data: []byte("print(1)\n")},
	"commands/_lib.py":    {Data: []byte("x = 1\n")},
}}

func TestBuiltinLayer(t *testing.T) {
	l, err := profile.Load(blender, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	if l.Profile.Title == "" || l.Layers[0].Path != "built-in:config.json" {
		t.Fatalf("loaded: %+v", l)
	}
	entries, err := fs.ReadDir(l.CommandDirs[0].FS, ".")
	if err != nil || len(entries) != 2 {
		t.Fatalf("built-in commands: %v %v", entries, err)
	}
	// An FS without config.json is no built-in layer.
	if _, err := profile.Load(profile.App{Name: "blender", FS: fstest.MapFS{}}, opts(t)); err == nil || !strings.Contains(err.Error(), "no config for blender") {
		t.Fatalf("empty FS: %v", err)
	}
}

func TestEnvExpansion(t *testing.T) {
	o := opts(t)
	godot := profile.App{Name: "godot", FS: fstest.MapFS{"config.json": {Data: []byte(
		`{"host": {"address": "127.0.0.1:${EDITOR_CONSOLE_PORT:-9510}", "token": "${EDITOR_CONSOLE_TOKEN}"}}`)}}}
	t.Setenv("EDITOR_CONSOLE_PORT", "")
	l, _ := profile.Load(godot, o)
	if l.Profile.Host.Address != "127.0.0.1:9510" || l.Profile.Host.Token != "" {
		t.Fatalf("defaults: %+v", l.Profile.Host)
	}
	t.Setenv("EDITOR_CONSOLE_PORT", "9600")
	t.Setenv("EDITOR_CONSOLE_TOKEN", "s3cret")
	l, _ = profile.Load(godot, o)
	if l.Profile.Host.Address != "127.0.0.1:9600" || l.Profile.Host.Token != "s3cret" {
		t.Fatalf("env: %+v", l.Profile.Host)
	}
	// Raw keeps the reference, so `config show` never prints the secret.
	if l.Raw.Host.Token != "${EDITOR_CONSOLE_TOKEN}" {
		t.Fatalf("raw: %q", l.Raw.Host.Token)
	}
}

func TestHomeProjectDirListedOnce(t *testing.T) {
	home := t.TempDir()
	o := profile.Options{ConfigDir: filepath.Join(home, ".mcp-sh"), ProjectDir: home}
	l, err := profile.Load(blender, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.CommandDirs) != 2 {
		t.Fatalf("command dirs: %+v", l.CommandDirs)
	}
}

func TestLayering(t *testing.T) {
	o := opts(t)
	write(t, profile.UserConfigPath("blender", o), `{
		"timeout": 30,
		"mcpServers": {
			"blender-mcp": {"command": "/opt/blender-mcp", "exec": {"tool": "x", "param": "code", "lang": "python"}},
			"extra": {"type": "http", "url": "http://localhost:${EXTRA_PORT:-7000}/mcp"}
		}
	}`)
	write(t, profile.ProjectConfigPath("blender", o), `{
		"title": "this project's Blender",
		"mcpServers": {"extra": {"disabled": true}}
	}`)
	l, err := profile.Load(blender, o)
	if err != nil {
		t.Fatal(err)
	}
	p := l.Profile
	if p.Title != "this project's Blender" || p.Timeout != 30 || p.Instructions == "" {
		t.Errorf("scalars: title %q timeout %d", p.Title, p.Timeout)
	}
	// User replaced the transport (args dropped with it) and exec; built-in defaults
	// survive. The project layer disabled extra.
	bm := p.MCPServers["blender-mcp"]
	if bm.Command != "/opt/blender-mcp" || len(bm.Args) != 0 || bm.Defaults["user_prompt"] != "" || bm.Exec.Tool != "x" {
		t.Errorf("blender-mcp: %+v exec %+v", bm, bm.Exec)
	}
	if names := p.ServerNames(); strings.Join(names, ",") != "blender-mcp" {
		t.Errorf("enabled servers: %v", names)
	}
	if len(l.CommandDirs) != 3 || !strings.HasPrefix(l.CommandDirs[0].Label, "built-in") {
		t.Errorf("command dirs: %+v", l.CommandDirs)
	}
	layers := profile.Layers(blender, o)
	if !layers[0].Exists || !layers[1].Exists || !layers[2].Exists {
		t.Errorf("layers: %+v", layers)
	}

	sources, err := l.Sources("v")
	if err != nil || len(sources) != 2 {
		t.Fatalf("sources: %v %v", sources, err)
	}
	if _, ok := sources[0].(*mcphost.Server); !ok {
		t.Errorf("first source: %T", sources[0])
	}
	if _, ok := sources[1].(*scripts.Source); !ok {
		t.Errorf("second source: %T", sources[1])
	}
}

func TestUserOnlyApp(t *testing.T) {
	o := opts(t)
	write(t, profile.UserConfigPath("houdini", o), `{"title": "Houdini", "host": {"address": "127.0.0.1:${HOUDINI_PORT:-9700}"}}`)
	l, err := profile.Load(profile.App{Name: "houdini"}, o)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := l.Sources("v")
	if err != nil || len(srcs) != 1 {
		t.Fatalf("%v %v", srcs, err)
	}
	if c, ok := srcs[0].(*host.Client); !ok || c.Addr != "127.0.0.1:9700" {
		t.Fatalf("host: %#v", srcs[0])
	}
	if l.Profile.Title != "Houdini" || len(l.CommandDirs) != 2 {
		t.Fatalf("loaded: %+v", l)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		`{"tittle": "typo"}`:                                `unknown field "tittle"`,
		`{"mcpServers": {"x": {}}}`:                         "needs a command or a url",
		`{"mcpServers": {"x": {"type": "http"}}}`:           "type http needs a url",
		`{"mcpServers": {"x": {"type": "ws", "url": "u"}}}`: `unknown type "ws"`,
		`{"mcpServers": {"x": {"command": "c", "exec": {"tool": "t", "param": "p", "lang": "ruby"}}}}`:                           `unknown lang "ruby"`,
		`{"mcpServers": {"bad name": {"command": "c"}}}`:                                                                         "invalid server name",
		`{"mcpServers": {"x": {"command": "c", "exec": {"tool": "t", "lang": "python"}}}}`:                                       "needs tool, and either param or args",
		`{"mcpServers": {"x": {"command": "c", "exec": {"tool": "t", "param": "p", "args": {"a": "$code"}, "lang": "python"}}}}`: "either param or args",
		`{"mcpServers": {"x": {"command": "c", "exec": {"tool": "t", "args": {"a": ["code"]}, "lang": "python"}}}}`:              `exec.args: no "$code"`,
		`{"mcpServers": {"x": {"command": "c", "exec": {"tool": "t", "param": "p", "lang": "python", "outputJq": "join("}}}}`:    "exec.outputJq:",
		`{"mcpServers": {"x": {"command": "c", "exec": {"tool": "t", "param": "p", "lang": "python", "errorJq": ".["}}}}`:        "exec.errorJq:",
	}
	cases[`{"host": {"address": "a:1", "exec": {"lang": "python"}}}`] = "host.exec: needs command"
	cases[`{"host": {"address": "a:1", "exec": {"command": "python", "lang": "ruby"}}}`] = `host.exec: unknown lang "ruby"`
	for cfg, want := range cases {
		o := opts(t)
		write(t, profile.UserConfigPath("custom", o), cfg)
		_, err := profile.Load(profile.App{Name: "custom"}, o)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", cfg, err, want)
		}
	}
	o := opts(t)
	if _, err := profile.Load(profile.App{Name: "nope"}, o); err == nil || !strings.Contains(err.Error(), "no config for nope: expected "+profile.UserConfigPath("nope", o)) {
		t.Errorf("missing: %v", err)
	}
	if _, err := profile.Load(profile.App{Name: "../etc"}, opts(t)); err == nil {
		t.Error("path traversal accepted")
	}
}

func TestEditing(t *testing.T) {
	o := opts(t)
	path, created, err := profile.EnsureUserConfig("blender", "mcp-sh-blender", o)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "commands")); err != nil {
		t.Fatal("commands dir not created")
	}
	if _, created, _ := profile.EnsureUserConfig("blender", "mcp-sh-blender", o); created {
		t.Fatal("overwrote existing config")
	}
	// Keys the editor does not model survive edits.
	raw, _ := os.ReadFile(path)
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["timeout"] = 45
	b, _ := json.Marshal(m)
	os.WriteFile(path, b, 0o644)

	if err := profile.AddServer(path, "assets", &profile.Server{Command: "npx", Args: []string{"-y", "asset-mcp"}, Env: map[string]string{"K": "V"}}); err != nil {
		t.Fatal(err)
	}
	l, err := profile.Load(blender, o)
	if err != nil {
		t.Fatal(err)
	}
	if l.Profile.Timeout != 45 || l.Profile.MCPServers["assets"].Args[1] != "asset-mcp" || l.Profile.MCPServers["blender-mcp"] == nil {
		t.Fatalf("after add: %+v", l.Profile)
	}
	if err := profile.RemoveServer(path, "assets"); err != nil {
		t.Fatal(err)
	}
	if err := profile.RemoveServer(path, "assets"); err == nil {
		t.Fatal("removing twice should fail")
	}
	l, _ = profile.Load(blender, o)
	if _, ok := l.Profile.MCPServers["assets"]; ok || l.Profile.Comment == "" {
		t.Fatalf("after remove: %+v", l.Profile.MCPServers)
	}
}

func TestImport(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	write(t, filepath.Join(project, ".mcp.json"), `{"mcpServers": {"proj-srv": {"type": "stdio", "command": "proj", "args": ["a"], "env": {"X": "1"}}}}`)
	write(t, filepath.Join(home, ".claude.json"), `{
		"numStartups": 3,
		"mcpServers": {"blender": {"type": "stdio", "command": "uvx", "args": ["blender-mcp"]}},
		"projects": {"`+filepath.ToSlash(project)+`": {"mcpServers": {"local-srv": {"type": "http", "url": "http://x/mcp", "headers": {"A": "b"}}}}}
	}`)
	sources := profile.ImportSources(project, home)
	if len(sources) != 3 {
		t.Fatalf("sources: %+v", sources)
	}
	s, label, err := profile.FindImport(sources, "blender")
	if err != nil || s.Command != "uvx" || s.Args[0] != "blender-mcp" || !strings.Contains(label, "(user)") {
		t.Fatalf("blender: %+v %q %v", s, label, err)
	}
	if s, _, err := profile.FindImport(sources, "local-srv"); err != nil || s.URL != "http://x/mcp" || s.Headers["A"] != "b" {
		t.Fatalf("local: %+v %v", s, err)
	}
	if _, _, err := profile.FindImport(sources, "nope"); err == nil || !strings.Contains(err.Error(), "found: [blender local-srv proj-srv]") {
		t.Fatalf("missing: %v", err)
	}
}

func TestCommandDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("TOOLS", "/opt/tools")
	rooted, _ := filepath.Abs("/opt/tools/blender")
	o := opts(t)
	userDir := filepath.Dir(profile.UserConfigPath("blender", o))
	write(t, profile.UserConfigPath("blender", o), `{"commandDirs": ["~/rbx", "${TOOLS}/blender", "mine", "~/rbx/../rbx"]}`)
	write(t, profile.ProjectConfigPath("blender", o), `{"commandDirs": ["../tools/commands"]}`)
	l, err := profile.Load(blender, o)
	if err != nil {
		t.Fatal(err)
	}
	type dir struct {
		label    string
		required bool
	}
	var got []dir
	for _, d := range l.CommandDirs {
		got = append(got, dir{d.Label, d.Required})
	}
	want := []dir{
		{"built-in blender commands", false},
		{filepath.Join(userDir, "commands"), false},
		{filepath.Join(home, "rbx"), true}, // its duplicate, ~/rbx/../rbx, is dropped
		{rooted, true},                     // C:\opt\tools\blender on Windows
		{filepath.Join(userDir, "mine"), true},
		{filepath.Join(o.ProjectDir, ".mcp-sh", "blender", "commands"), false},
		{filepath.Join(o.ProjectDir, "tools", "commands"), true},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("command dirs:\n%v\nwant:\n%v", got, want)
	}
	if strings.Join(l.Raw.CommandDirs, ",") != "~/rbx,${TOOLS}/blender,mine,~/rbx/../rbx,../tools/commands" {
		t.Errorf("raw: %v", l.Raw.CommandDirs)
	}
	if len(l.Layers[1].ExtraCommandDirs) != 4 || len(l.Layers[2].ExtraCommandDirs) != 1 {
		t.Errorf("layers: %+v", l.Layers)
	}

	builtin := profile.App{Name: "x", FS: fstest.MapFS{"config.json": {Data: []byte(`{"commandDirs": ["/a"]}`)}}}
	if _, err := profile.Load(builtin, opts(t)); err == nil || !strings.Contains(err.Error(), "commandDirs is for user and project configs") {
		t.Errorf("built-in commandDirs: %v", err)
	}
	o = opts(t)
	write(t, profile.UserConfigPath("blender", o), `{"commandDirs": [" "]}`)
	if _, err := profile.Load(blender, o); err == nil || !strings.Contains(err.Error(), "empty entry") {
		t.Errorf("empty entry: %v", err)
	}
}

func TestEditCommandDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write(t, path, `{"timeout": 9, "futureKey": true}`)
	if err := profile.AddCommandDir(path, "~/a"); err != nil {
		t.Fatal(err)
	}
	if err := profile.AddCommandDir(path, "/b"); err != nil {
		t.Fatal(err)
	}
	if err := profile.AddCommandDir(path, "/b"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("duplicate: %v", err)
	}
	if err := profile.RemoveCommandDir(path, "~/a"); err != nil {
		t.Fatal(err)
	}
	if err := profile.RemoveCommandDir(path, "/c"); err == nil || !strings.Contains(err.Error(), "commandDirs: /b") {
		t.Fatalf("missing entry: %v", err)
	}
	var m map[string]any
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &m)
	if m["futureKey"] != true || m["timeout"] != float64(9) || len(m["commandDirs"].([]any)) != 1 {
		t.Fatalf("file: %s", b)
	}
	profile.RemoveCommandDir(path, "/b")
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "commandDirs") {
		t.Fatalf("empty list kept: %s", b)
	}
}

func TestExecTemplateAndImageDir(t *testing.T) {
	o := opts(t)
	write(t, profile.UserConfigPath("gimp", o), `{"mcpServers": {"gimp": {
		"command": "uv", "args": ["run", "gimp_mcp_server.py"],
		"exec": {"tool": "call_api", "lang": "python",
			"args": {"api_path": "exec", "args": ["pyGObject-console", ["$code"]]},
			"outputJq": "join(\"\")", "errorPrefix": "Error: ", "errorJq": "."}
	}}}`)
	l, err := profile.Load(profile.App{Name: "gimp"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(o.ConfigDir, "gimp", "images"); l.ImageDir != want {
		t.Fatalf("image dir %q, want %q", l.ImageDir, want)
	}
	srcs, err := l.Sources("v")
	if err != nil {
		t.Fatal(err)
	}
	src, ok := srcs[len(srcs)-1].(*scripts.Source)
	if !ok || len(src.Bindings) != 1 {
		t.Fatalf("sources: %#v", srcs)
	}
	b := src.Bindings[0]
	if b.Tool != "call_api" || b.Param != "" || b.ErrorPrefix != "Error: " || b.OutputJQ == nil || b.ErrorJQ == nil {
		t.Fatalf("binding: %+v", b)
	}
	if got, _ := json.Marshal(b.Args); string(got) != `{"api_path":"exec","args":["pyGObject-console",["$code"]]}` {
		t.Fatalf("args: %s", got)
	}
}

func TestHostExec(t *testing.T) {
	app := profile.App{Name: "gimp", FS: fstest.MapFS{"config.json": {Data: []byte(`{
		"host": {"address": "127.0.0.1:${MCP_SH_GIMP_PORT:-9520}", "exec": {"command": "python", "lang": "python"}}
	}`)}}}
	o := opts(t)
	// A user layer that only changes the address keeps the built-in exec.
	write(t, profile.UserConfigPath("gimp", o), `{"host": {"address": "127.0.0.1:9999"}}`)
	l, err := profile.Load(app, o)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := l.Sources("v")
	if err != nil || len(srcs) != 2 {
		t.Fatalf("sources: %v %v", srcs, err)
	}
	client, ok := srcs[0].(*host.Client)
	if !ok || client.Addr != "127.0.0.1:9999" {
		t.Fatalf("host: %#v", srcs[0])
	}
	src, ok := srcs[1].(*scripts.Source)
	if !ok || len(src.Bindings) != 1 {
		t.Fatalf("scripts: %#v", srcs[1])
	}
	if b := src.Bindings[0]; b.Host != client || b.Tool != "python" || b.Lang != "python" || b.Server != nil {
		t.Fatalf("binding: %+v", b)
	}
}
