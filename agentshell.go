// Package agentshell is the CLI and MCP server behind an app's shell binary: a sandboxed
// bash shell over a running program, built from the app's layered config (see package
// profile). Each app is its own module and binary that embeds its config.json and
// commands/ and calls Main:
//
//	//go:embed config.json commands
//	var appFS embed.FS
//
//	func main() {
//		agentshell.Main(agentshell.Config{Name: "blender-shell", App: "blender", FS: appFS})
//	}
//
// `<binary> setup` registers it with Claude Code and prints what the app needs.
package agentshell

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brohd11/goutil/selfupdate"
	"github.com/brohd11/goutil/shellquote"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/builtins"
	"github.com/brohd11/agent-shell/engine"
	"github.com/brohd11/agent-shell/engine/shengine"
	"github.com/brohd11/agent-shell/mcpserver"
	"github.com/brohd11/agent-shell/profile"
	"github.com/brohd11/agent-shell/shell"
)

// Builtin is a Go-side command an app binary adds to its shell.
type Builtin = engine.Command

// Invocation is one call of a Builtin.
type Invocation = engine.Invocation

// Subcommand is an app-specific CLI verb, such as godot-shell's `addon`. Core verbs win
// over a Subcommand of the same name.
type Subcommand struct {
	Name    string // first argument that selects it, e.g. "addon"
	Usage   string // help usage after the binary name, e.g. "addon install|status|remove [DIR]"
	Summary string // help description
	// Run gets the arguments after Name and returns the exit code.
	Run func(args []string, stdout, stderr io.Writer) int
}

type Config struct {
	Name    string // binary and MCP server name, e.g. "blender-shell"
	App     string // config folder name, e.g. "blender" for ~/.agent-shell/blender/ (default: Name)
	Version string
	// UpdateRepo ("owner/repo") enables `update` via GitHub releases.
	UpdateRepo string
	// FS is the built-in config layer: config.json and an optional commands/ folder,
	// usually an embed.FS. Nil means the app is configured by user files only.
	FS fs.FS
	// Builtins are extra Go-side commands; they win over every other command source.
	Builtins []Builtin
	// Subcommands are extra CLI verbs, listed in --help after the core ones.
	Subcommands []Subcommand
	// Options overrides where config files are looked up (tests).
	Options profile.Options
	// Claude is the client CLI used by `setup` (default "claude"; tests use a fake).
	Claude string
}

// Main runs the CLI and exits.
func Main(cfg Config) {
	os.Exit(run(cfg, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type cli struct {
	cfg            Config
	stdin          io.Reader
	stdout, stderr io.Writer
}

func (c *cli) printf(format string, a ...any)  { fmt.Fprintf(c.stdout, format, a...) }
func (c *cli) eprintf(format string, a ...any) { fmt.Fprintf(c.stderr, format, a...) }

func run(cfg Config, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if cfg.Name == "" {
		cfg.Name = "agent-shell"
	}
	if cfg.App == "" {
		cfg.App = cfg.Name
	}
	c := &cli{cfg: cfg, stdin: stdin, stdout: stdout, stderr: stderr}

	sub, rest := "", []string(nil)
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "-h", "--help", "help":
		c.printf("%s", c.helpText())
		return 0
	case "--version", "version":
		c.printf("%s\n", cfg.Version)
		return 0
	case "update":
		if cfg.UpdateRepo == "" {
			break
		}
		cmd := selfupdate.NewUpdateCommand(cfg.UpdateRepo, cfg.Name, cfg.Version)
		cmd.SetArgs(rest)
		if cmd.Execute() != nil {
			return 1
		}
		return 0
	case "setup":
		return c.withApp(func(l *profile.Loaded) int { return c.setup(l, rest) })
	case "", "serve":
		return c.withApp(c.serve)
	case "run":
		return c.withApp(func(l *profile.Loaded) int { return c.runScript(l, rest) })
	case "commands":
		return c.withApp(func(l *profile.Loaded) int { return c.commands(l, rest) })
	case "mcp":
		return c.withApp(func(l *profile.Loaded) int { return c.mcp(l, rest) })
	case "config":
		return c.withApp(func(l *profile.Loaded) int { return c.config(l, rest) })
	}
	for _, s := range cfg.Subcommands {
		if s.Name == sub && sub != "" {
			return s.Run(rest, stdout, stderr)
		}
	}
	c.eprintf("unknown command %q\nRun %s --help for usage.\n", sub, cfg.Name)
	return 2
}

func (c *cli) helpText() string {
	n, app := c.cfg.Name, c.cfg.App
	usage := [][2]string{
		{"", "Start the MCP server over stdio"},
		{"setup [--scope user|project|local] [--name NAME] [--print]", "Create the user config and register with Claude Code"},
		{"run SCRIPT", `Run a script once ("-" reads it from stdin)`},
		{"commands", "List the commands and their sources"},
		{"commands add|remove DIR [--project]", "Add or remove a folder of your own script commands"},
		{"mcp list|add|remove|import ...", "Manage the upstream MCP servers"},
		{"config path|show", "Show config files, or the merged config"},
	}
	for _, s := range c.cfg.Subcommands {
		usage = append(usage, [2]string{s.Usage, s.Summary})
	}
	if c.cfg.UpdateRepo != "" {
		usage = append(usage, [2]string{"update [--check]", "Install an update, or only check for one"})
	}
	var b strings.Builder
	fmt.Fprintf(&b, `%s: a sandboxed bash shell over a running program, served over MCP.
Its config says which program: a native host, the app's existing MCP servers, and
your script commands.

Usage:
`, n)
	// Descriptions line up after the longest short form; longer forms wrap.
	width := len(n) + len(" commands add|remove DIR [--project]")
	for _, u := range usage {
		cmd := strings.TrimSpace(n + " " + u[0])
		if len(cmd) > width {
			fmt.Fprintf(&b, "  %s\n  %-*s  %s\n", cmd, width, "", u[1])
		} else {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, cmd, u[1])
		}
	}
	fmt.Fprintf(&b, `
Options:
  -h, --help           Show this help
  --version            Print the version

Config:
  user      %[1]s  (+ commands/)
  project   %[2]s  (+ .agent-shell/%[3]s/commands/)
Environment: AGENT_SHELL_CONFIG_DIR, AGENT_SHELL_TIMEOUT (seconds)
`, profile.UserConfigPath(app, c.cfg.Options), filepath.Join(".agent-shell", app+".json"), app)
	return b.String()
}

func (c *cli) withApp(fn func(*profile.Loaded) int) int {
	l, err := profile.Load(profile.App{Name: c.cfg.App, FS: c.cfg.FS}, c.cfg.Options)
	if err != nil {
		c.eprintf("%v\n", err)
		return 2
	}
	return fn(l)
}

// newShell builds the runnable shell for a loaded config.
func (c *cli) newShell(l *profile.Loaded) (*shell.Shell, error) {
	p := l.Profile
	sources, err := l.Sources(c.cfg.Version)
	if err != nil {
		return nil, err
	}
	root := p.Root
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("root: %q is not a directory", root)
		}
		root = abs
	}
	timeout := time.Duration(p.Timeout) * time.Second
	if s := os.Getenv("AGENT_SHELL_TIMEOUT"); s != "" {
		secs, err := strconv.Atoi(s)
		if err != nil || secs <= 0 {
			return nil, fmt.Errorf("AGENT_SHELL_TIMEOUT: expected a positive number of seconds, got %q", s)
		}
		timeout = time.Duration(secs) * time.Second
	}
	return &shell.Shell{
		Sources: sources,
		Engine: shengine.New(shengine.Options{
			Root: root,
			Env:  []string{"HOME=/", "AGENT_SHELL=" + c.cfg.Name, "AGENT_SHELL_APP=" + l.Name},
		}),
		Extra:     c.cfg.Builtins,
		Timeout:   timeout,
		MaxOutput: p.MaxOutput,
	}, nil
}

func fileAccess(l *profile.Loaded) string {
	if l.Profile.Root != "" {
		return "read/write inside a sandbox directory, seen as /"
	}
	return "disabled (pipe data between commands or use variables)"
}

// instructions adds the config's command groups to its own guidance, so the agent
// knows which namespaces exist before listing commands.
func instructions(l *profile.Loaded) string {
	p := l.Profile
	var parts []string
	if p.Instructions != "" {
		parts = append(parts, p.Instructions)
	}
	var groups []string
	if p.Host != nil && !p.Host.Disabled && p.Host.Address != "" {
		groups = append(groups, "the app's own commands (top level)")
	}
	for _, name := range p.ServerNames() {
		groups = append(groups, fmt.Sprintf("'%s' (an MCP server: tools are subcommands, see 'help %s')", name, name))
	}
	for _, name := range p.ServerNames() {
		if e := p.MCPServers[name].Exec; e != nil {
			groups = append(groups, fmt.Sprintf("script commands (%s, run via %s)", e.Lang, name))
			break
		}
	}
	if len(groups) > 0 {
		parts = append(parts, "Command groups: "+strings.Join(groups, "; ")+".")
	}
	return strings.Join(parts, "\n\n")
}

func (c *cli) serve(l *profile.Loaded) int {
	sh, err := c.newShell(l)
	if err != nil {
		c.eprintf("%v\n", err)
		return 2
	}
	defer sh.Close()
	title := l.Profile.Title
	if title == "" {
		title = l.Name
	}
	server := mcpserver.New(sh, mcpserver.Options{
		Name: c.cfg.Name, Version: c.cfg.Version, Title: title,
		Instructions: instructions(l), FileAccess: fileAccess(l),
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		c.eprintf("%v\n", err)
		return 1
	}
	return 0
}

func (c *cli) runScript(l *profile.Loaded, args []string) int {
	if len(args) != 1 {
		c.eprintf("usage: %s run \"<script>\"\n", c.cfg.Name)
		return 2
	}
	script := args[0]
	if script == "-" {
		b, err := io.ReadAll(c.stdin)
		if err != nil {
			c.eprintf("%v\n", err)
			return 1
		}
		script = string(b)
	}
	sh, err := c.newShell(l)
	if err != nil {
		c.eprintf("%v\n", err)
		return 2
	}
	defer sh.Close()
	res := sh.Run(context.Background(), script, 0)
	io.WriteString(c.stdout, res.Stdout)
	io.WriteString(c.stderr, res.Stderr)
	for _, w := range res.Warnings {
		c.eprintf("[warning] %s\n", w)
	}
	if res.Truncated {
		c.eprintf("[output truncated]\n")
	}
	if res.Err != nil {
		c.eprintf("%v\n", res.Err)
	}
	return res.ExitCode
}

func (c *cli) commands(l *profile.Loaded, args []string) int {
	if len(args) > 0 {
		return c.commandDirs(l, args)
	}
	sh, err := c.newShell(l)
	if err != nil {
		c.eprintf("%v\n", err)
		return 2
	}
	defer sh.Close()
	ctx, cancel := context.WithTimeout(context.Background(), sh.Timeout+time.Minute)
	defer cancel()
	reg, statuses, err := sh.Commands(ctx)
	c.printf("%s", shell.FormatSources(statuses))
	if err != nil {
		return 1
	}
	c.printf("\n%s", builtins.FormatList(reg))
	return 0
}

// commandDirs edits the commandDirs list of the user config (or the project config with
// --project): `commands add DIR` and `commands remove DIR`.
func (c *cli) commandDirs(l *profile.Loaded, args []string) int {
	usage := func() int {
		c.eprintf("usage: %s commands add|remove DIR [--project]\n", c.cfg.Name)
		return 2
	}
	sub, args := args[0], args[1:]
	target := profile.UserConfigPath(l.Name, c.cfg.Options)
	var dir string
	for _, a := range args {
		switch {
		case a == "--project":
			target = profile.ProjectConfigPath(l.Name, c.cfg.Options)
		case strings.HasPrefix(a, "-") || dir != "":
			c.eprintf("unexpected argument %q\n", a)
			return usage()
		default:
			dir = a
		}
	}
	if dir == "" || (sub != "add" && sub != "remove") {
		return usage()
	}
	// A path typed relative to the working directory is stored absolute, since the
	// config resolves relative entries against its own folder. ~ and ${VAR} are kept.
	entry := dir
	if !strings.HasPrefix(dir, "~") && !strings.Contains(dir, "${") && !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			entry = abs
		}
	}
	if sub == "remove" {
		err := profile.RemoveCommandDir(target, dir)
		if err != nil && entry != dir {
			err = profile.RemoveCommandDir(target, entry)
		}
		if err != nil {
			c.eprintf("%v\n", err)
			return 1
		}
		c.printf("Removed %s from commandDirs in %s\n", dir, target)
		return 0
	}
	if err := profile.AddCommandDir(target, entry); err != nil {
		c.eprintf("%v\n", err)
		return 1
	}
	c.printf("Added %s to commandDirs in %s\n", entry, target)
	if info, err := os.Stat(profile.ResolveDir(entry, filepath.Dir(target))); err != nil || !info.IsDir() {
		c.eprintf("warning: %s is not a folder yet\n", entry)
	}
	return 0
}

func (c *cli) setup(l *profile.Loaded, args []string) int {
	scope, regName, printOnly := "user", "", false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--print":
			printOnly = true
		case a == "--scope" || a == "--name":
			if i+1 >= len(args) {
				c.eprintf("%s needs a value\n", a)
				return 2
			}
			if a == "--scope" {
				scope = args[i+1]
			} else {
				regName = args[i+1]
			}
			i++
		default:
			c.eprintf("unexpected argument %q\nusage: %s setup [--scope user|project|local] [--name NAME] [--print]\n", a, c.cfg.Name)
			return 2
		}
	}
	if scope != "user" && scope != "project" && scope != "local" {
		c.eprintf("--scope must be user, project or local\n")
		return 2
	}
	if regName == "" {
		regName = c.cfg.Name
	}

	path, created, err := profile.EnsureUserConfig(l.Name, c.cfg.Name, c.cfg.Options)
	if err != nil {
		c.eprintf("creating user config: %v\n", err)
		return 1
	}
	if created {
		c.printf("Created %s (and commands/ next to it) for your overrides.\n", path)
	} else {
		c.printf("Using existing %s.\n", path)
	}

	exe, err := os.Executable()
	if err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
	}
	if err != nil || exe == "" {
		exe = c.cfg.Name
	}
	claude := c.cfg.Claude
	if claude == "" {
		claude = "claude"
	}
	register := []string{claude, "mcp", "add", "-s", scope, regName, "--", exe}
	_, lookErr := exec.LookPath(claude)
	switch {
	case printOnly || lookErr != nil:
		if lookErr != nil && !printOnly {
			c.printf("\n%s was not found on PATH; register the server yourself:\n", claude)
		} else {
			c.printf("\nRegister the server with:\n")
		}
		c.printf("  %s\n", shellquote.JoinMinimal(register))
	default:
		c.printf("\nRegistering: %s\n", shellquote.JoinMinimal(register))
		cmd := exec.Command(register[0], register[1:]...)
		cmd.Stdout, cmd.Stderr = c.stdout, c.stderr
		if err := cmd.Run(); err != nil {
			c.eprintf("Registration failed (%v). If %q already exists, remove it first:\n  %s mcp remove -s %s %s\n", err, regName, claude, scope, regName)
			return 1
		}
	}
	if setup := strings.TrimSpace(l.Profile.Setup); setup != "" {
		c.printf("\nIn the app:\n%s\n", indent(setup, "  "))
	}
	c.printf("\nCheck it with:\n  %s commands\n", c.cfg.Name)
	return 0
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func (c *cli) mcp(l *profile.Loaded, args []string) int {
	if len(args) == 0 {
		c.eprintf("usage: %s mcp list | add NAME [--url URL | -- COMMAND ARGS...] | remove NAME | import NAME [--as NAME]\n", c.cfg.Name)
		return 2
	}
	sub, args := args[0], args[1:]
	if sub == "list" {
		names := make([]string, 0, len(l.Raw.MCPServers))
		for n := range l.Raw.MCPServers {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			c.printf("no MCP servers configured\n")
		}
		for _, n := range names {
			s := l.Raw.MCPServers[n]
			if s == nil {
				continue
			}
			where := s.URL
			if where == "" {
				where = shellquote.JoinMinimal(append([]string{s.Command}, s.Args...))
			}
			line := fmt.Sprintf("%-16s %s", n, where)
			if s.Exec != nil {
				line += fmt.Sprintf("  [script commands: %s via %s]", s.Exec.Lang, s.Exec.Tool)
			}
			if s.Disabled {
				line += "  (disabled)"
			}
			c.printf("%s\n", line)
		}
		return 0
	}

	// Edits go to the user config, or the project config with --project.
	target := profile.UserConfigPath(l.Name, c.cfg.Options)
	var name, url, typ, as string
	var command []string
	env, headers := map[string]string{}, map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, bool) {
			if i+1 >= len(args) {
				c.eprintf("%s needs a value\n", a)
				return "", false
			}
			i++
			return args[i], true
		}
		var ok = true
		switch a {
		case "--project":
			target = profile.ProjectConfigPath(l.Name, c.cfg.Options)
		case "--url":
			url, ok = value()
		case "--type":
			typ, ok = value()
		case "--as":
			as, ok = value()
		case "--env", "--header":
			var kv string
			if kv, ok = value(); ok {
				sep := "="
				if a == "--header" {
					sep = ":"
				}
				k, v, found := strings.Cut(kv, sep)
				if !found {
					c.eprintf("%s expects KEY%sVALUE\n", a, sep)
					return 2
				}
				if a == "--env" {
					env[k] = v
				} else {
					headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
		case "--":
			command = args[i+1:]
			i = len(args)
		default:
			if strings.HasPrefix(a, "-") || name != "" {
				c.eprintf("unexpected argument %q\n", a)
				return 2
			}
			name = a
		}
		if !ok {
			return 2
		}
	}
	if name == "" {
		c.eprintf("mcp %s needs a server NAME\n", sub)
		return 2
	}

	switch sub {
	case "add":
		s := &profile.Server{Type: typ, URL: url}
		if len(env) > 0 {
			s.Env = env
		}
		if len(headers) > 0 {
			s.Headers = headers
		}
		if len(command) > 0 {
			s.Command, s.Args = command[0], command[1:]
		}
		if (s.URL == "") == (s.Command == "") {
			c.eprintf("give either --url URL or -- COMMAND [ARGS...]\n")
			return 2
		}
		if s.URL != "" && s.Type == "" {
			s.Type = "http"
		}
		if err := profile.AddServer(target, name, s); err != nil {
			c.eprintf("%v\n", err)
			return 1
		}
		c.printf("Added %s to %s\n", name, target)
	case "remove":
		if err := profile.RemoveServer(target, name); err != nil {
			c.eprintf("%v\n", err)
			if _, inConfig := l.Raw.MCPServers[name]; inConfig {
				c.eprintf("It comes from another layer; to turn it off here, add: \"mcpServers\": {%q: {\"disabled\": true}}\n", name)
			}
			return 1
		}
		c.printf("Removed %s from %s\n", name, target)
	case "import":
		home, _ := os.UserHomeDir()
		project := c.cfg.Options.ProjectDir
		if project == "" {
			project, _ = os.Getwd()
		}
		s, from, err := profile.FindImport(profile.ImportSources(project, home), name)
		if err != nil {
			c.eprintf("%v\n", err)
			return 1
		}
		if as == "" {
			as = name
		}
		if err := profile.AddServer(target, as, s); err != nil {
			c.eprintf("%v\n", err)
			return 1
		}
		c.printf("Imported %s from %s into %s as %s\n", name, from, target, as)
	default:
		c.eprintf("unknown mcp command %q (list, add, remove, import)\n", sub)
		return 2
	}
	return 0
}

func (c *cli) config(l *profile.Loaded, args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "path":
		for _, layer := range l.Layers {
			mark := "missing"
			if layer.Exists {
				mark = "found"
			}
			c.printf("%-9s %-7s %s\n", layer.Kind, mark, layer.Path)
			c.printf("%-9s %-7s %s\n", "", "", layer.CommandsDir)
			for _, d := range layer.ExtraCommandDirs {
				mark := "missing"
				if info, err := os.Stat(d); err == nil && info.IsDir() {
					mark = "found"
				}
				c.printf("%-9s %-7s %s\n", "", mark, d)
			}
		}
		return 0
	case "show":
		b, err := json.MarshalIndent(l.Raw, "", "  ")
		if err != nil {
			c.eprintf("%v\n", err)
			return 1
		}
		c.printf("%s\n", b)
		return 0
	}
	c.eprintf("usage: %s config path|show\n", c.cfg.Name)
	return 2
}
