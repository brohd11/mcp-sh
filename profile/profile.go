// Package profile describes one app's shell: an optional native host, upstream MCP
// servers, script commands and limits. The config is JSON in the shape of `.mcp.json`
// (an "mcpServers" map) plus agent-shell keys, layered:
//
//	built-in   embedded in the app's binary (App.FS: config.json, commands/)
//	user       <config dir>/<app>/config.json and <config dir>/<app>/commands/
//	project    <project>/.agent-shell/<app>.json and <project>/.agent-shell/<app>/commands/
//
// Later layers win: scalar keys override and command folders stack. mcpServers entries
// merge by name: giving any transport key (type, command, args, env, url, headers)
// replaces the transport as a unit, while exec, defaults, hideTools and disabled
// override individually, so pointing a built-in server at another command keeps its
// agent-shell options. Strings in host, mcpServers and root expand ${VAR} and
// ${VAR:-default}.
package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/brohd11/agent-shell/host/mcphost"
	"github.com/brohd11/agent-shell/host/scripts"
)

// App is the built-in layer an app's binary embeds.
type App struct {
	Name string // config folder: <config dir>/<Name>/, <project>/.agent-shell/<Name>.json
	// FS holds config.json and an optional commands/ folder; nil means no built-in
	// layer. Directory embeds skip files starting with "_", so embed command
	// libraries (commands/_lib.*) explicitly.
	FS fs.FS
}

type Profile struct {
	Comment      string `json:"$comment,omitempty"`
	Title        string `json:"title,omitempty"`        // names the app for the agent: "the running Blender session"
	Instructions string `json:"instructions,omitempty"` // app-specific guidance for the agent
	Setup        string `json:"setup,omitempty"`        // printed by `setup`: what the app still needs

	Host       *Host              `json:"host,omitempty"`
	MCPServers map[string]*Server `json:"mcpServers,omitempty"`

	// CommandDirs are extra folders of script commands (user and project configs only).
	// ${VAR} and a leading ~/ expand; relative paths are relative to the config file's
	// folder. Entries from every layer stack.
	CommandDirs []string `json:"commandDirs,omitempty"`

	Root      string `json:"root,omitempty"`      // directory exposed to scripts as "/"; empty: no file access
	Timeout   int    `json:"timeout,omitempty"`   // default script timeout, seconds
	MaxOutput int    `json:"maxOutput,omitempty"` // per-stream output cap, bytes
}

// Host is a native agent-shell host (the TCP host protocol), like the Godot bridge.
type Host struct {
	Address  string `json:"address,omitempty"` // "127.0.0.1:9510"
	Token    string `json:"token,omitempty"`
	Hint     string `json:"hint,omitempty"` // shown when the host is unreachable
	Disabled bool   `json:"disabled,omitempty"`
}

// Server is an upstream MCP server: an `.mcp.json` entry plus agent-shell options.
type Server struct {
	Type    string            `json:"type,omitempty"` // stdio (default with command), http, sse
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	HideTools []string       `json:"hideTools,omitempty"`
	Defaults  map[string]any `json:"defaults,omitempty"` // tool args filled when not passed
	// ErrorPattern is a regexp; matching tool output counts as an error (exit 1).
	ErrorPattern string `json:"errorPattern,omitempty"`
	Exec         *Exec  `json:"exec,omitempty"` // runs script commands
	Disabled     bool   `json:"disabled,omitempty"`
}

// Exec binds script commands of one language to a server's code-execution tool.
type Exec struct {
	Tool         string `json:"tool"`  // "execute_blender_code"
	Param        string `json:"param"` // "code"
	Lang         string `json:"lang"`  // "python" or "luau"
	OutputPrefix string `json:"outputPrefix,omitempty"`
	ErrorPrefix  string `json:"errorPrefix,omitempty"`
	ErrorTrim    string `json:"errorTrim,omitempty"` // regexp removed from error text
}

// Layer is one config file that may contribute to a profile.
type Layer struct {
	Kind        string // "built-in", "user", "project"
	Path        string // file path ("built-in:config.json" for embedded)
	Exists      bool
	CommandsDir string // folder of script commands for this layer
	// ExtraCommandDirs are this layer's commandDirs, resolved (set by Load).
	ExtraCommandDirs []string
}

// Loaded is an app's config after layering.
type Loaded struct {
	Name string
	// Raw is the merged config before ${VAR} expansion; safe to print (no secrets
	// pulled from the environment).
	Raw Profile
	// Profile is Raw with variables expanded; use this to run.
	Profile     Profile
	Layers      []Layer
	CommandDirs []scripts.Dir
}

type Options struct {
	ProjectDir string // where to look for .agent-shell/; empty: the working directory
	ConfigDir  string // user config dir; empty: ConfigDir()
}

// ConfigDir is the user-level config folder shared by all apps:
// $AGENT_SHELL_CONFIG_DIR, else ~/.agent-shell.
func ConfigDir() string {
	if d := os.Getenv("AGENT_SHELL_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".agent-shell-config")
	}
	return filepath.Join(home, ".agent-shell")
}

func (o Options) configDir() string {
	if o.ConfigDir != "" {
		return o.ConfigDir
	}
	return ConfigDir()
}

func (o Options) projectDir() string {
	if o.ProjectDir != "" {
		return o.ProjectDir
	}
	wd, _ := os.Getwd()
	return wd
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// UserConfigPath is where an app's user-level config lives.
func UserConfigPath(name string, o Options) string {
	return filepath.Join(o.configDir(), name, "config.json")
}

// ProjectConfigPath is where an app's project-level config lives.
func ProjectConfigPath(name string, o Options) string {
	return filepath.Join(o.projectDir(), ".agent-shell", name+".json")
}

// Layers lists the files an app's config is built from, in precedence order.
func Layers(app App, o Options) []Layer {
	builtin := false
	if app.FS != nil {
		_, err := fs.Stat(app.FS, "config.json")
		builtin = err == nil
	}
	user := UserConfigPath(app.Name, o)
	project := ProjectConfigPath(app.Name, o)
	return []Layer{
		{Kind: "built-in", Path: "built-in:config.json", Exists: builtin, CommandsDir: "built-in:commands"},
		{Kind: "user", Path: user, Exists: fileExists(user), CommandsDir: filepath.Join(filepath.Dir(user), "commands")},
		{Kind: "project", Path: project, Exists: fileExists(project), CommandsDir: filepath.Join(filepath.Dir(project), app.Name, "commands")},
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// Load builds an app's config from its layers.
func Load(app App, o Options) (*Loaded, error) {
	if !validName.MatchString(app.Name) {
		return nil, fmt.Errorf("invalid app name %q", app.Name)
	}
	layers := Layers(app, o)
	if !layers[0].Exists && !layers[1].Exists && !layers[2].Exists {
		return nil, fmt.Errorf("no config for %s: expected %s", app.Name, layers[1].Path)
	}
	l := &Loaded{Name: app.Name, Layers: layers}
	for i := range layers {
		layer := &layers[i]
		if !layer.Exists {
			continue
		}
		var data []byte
		var err error
		if layer.Kind == "built-in" {
			data, err = fs.ReadFile(app.FS, "config.json")
		} else {
			data, err = os.ReadFile(layer.Path)
		}
		if err != nil {
			return nil, err
		}
		decoded, err := decodeInto(data, &l.Raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", layer.Path, err)
		}
		if len(decoded.CommandDirs) > 0 && layer.Kind == "built-in" {
			return nil, fmt.Errorf("%s: commandDirs is for user and project configs", layer.Path)
		}
		for _, d := range decoded.CommandDirs {
			if strings.TrimSpace(d) == "" {
				return nil, fmt.Errorf("%s: commandDirs: empty entry", layer.Path)
			}
			layer.ExtraCommandDirs = append(layer.ExtraCommandDirs, ResolveDir(d, filepath.Dir(layer.Path)))
		}
	}
	l.Profile = expand(l.Raw)
	if err := validate(l.Profile); err != nil {
		return nil, fmt.Errorf("%s config: %w", app.Name, err)
	}
	// Command folders, later ones winning: built-in, user commands/ then the user's
	// commandDirs, project commands/ then the project's commandDirs. Missing default
	// folders are skipped at scan time; missing commandDirs are reported.
	if app.FS != nil {
		if sub, err := fs.Sub(app.FS, "commands"); err == nil {
			l.CommandDirs = append(l.CommandDirs, scripts.Dir{Label: "built-in " + app.Name + " commands", FS: sub})
		}
	}
	var seen []string
	add := func(dir string, required bool) {
		// Run from the home folder, the project folder is the user folder: list it once.
		for _, s := range seen {
			if sameDir(s, dir) {
				return
			}
		}
		seen = append(seen, dir)
		l.CommandDirs = append(l.CommandDirs, scripts.Dir{Label: dir, FS: os.DirFS(dir), Required: required})
	}
	for _, layer := range layers[1:] {
		add(layer.CommandsDir, false)
		for _, d := range layer.ExtraCommandDirs {
			add(d, true)
		}
	}
	return l, nil
}

// ResolveDir expands ${VAR} and a leading ~/ in a commandDirs entry, and makes a
// relative path relative to base (the folder of the config file naming it).
func ResolveDir(dir, base string) string {
	dir = ExpandVars(dir)
	if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[1:])
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(base, dir)
	}
	return filepath.Clean(dir)
}

func sameDir(a, b string) bool {
	a, errA := filepath.Abs(a)
	b, errB := filepath.Abs(b)
	return errA == nil && errB == nil && a == b
}

// decodeInto merges one layer into p and returns the layer. Unknown keys are errors,
// to catch typos.
func decodeInto(data []byte, p *Profile) (*Profile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var layer Profile
	if err := dec.Decode(&layer); err != nil {
		return nil, err
	}
	merge(p, &layer)
	return &layer, nil
}

func merge(base, layer *Profile) {
	str := func(dst *string, src string) {
		if src != "" {
			*dst = src
		}
	}
	str(&base.Comment, layer.Comment)
	str(&base.Title, layer.Title)
	str(&base.Instructions, layer.Instructions)
	str(&base.Setup, layer.Setup)
	str(&base.Root, layer.Root)
	base.CommandDirs = append(base.CommandDirs, layer.CommandDirs...)
	if layer.Timeout != 0 {
		base.Timeout = layer.Timeout
	}
	if layer.MaxOutput != 0 {
		base.MaxOutput = layer.MaxOutput
	}
	if h := layer.Host; h != nil {
		if base.Host == nil {
			base.Host = &Host{}
		}
		str(&base.Host.Address, h.Address)
		str(&base.Host.Token, h.Token)
		str(&base.Host.Hint, h.Hint)
		base.Host.Disabled = base.Host.Disabled || h.Disabled
	}
	for name, s := range layer.MCPServers {
		if s == nil {
			continue
		}
		if base.MCPServers == nil {
			base.MCPServers = map[string]*Server{}
		}
		prev, ok := base.MCPServers[name]
		if !ok || prev == nil {
			c := *s
			base.MCPServers[name] = &c
			continue
		}
		m := *prev
		if s.Type != "" || s.Command != "" || s.Args != nil || s.Env != nil || s.URL != "" || s.Headers != nil {
			m.Type, m.Command, m.Args, m.Env, m.URL, m.Headers = s.Type, s.Command, s.Args, s.Env, s.URL, s.Headers
		}
		if s.HideTools != nil {
			m.HideTools = s.HideTools
		}
		if s.Defaults != nil {
			m.Defaults = s.Defaults
		}
		if s.ErrorPattern != "" {
			m.ErrorPattern = s.ErrorPattern
		}
		if s.Exec != nil {
			m.Exec = s.Exec
		}
		m.Disabled = m.Disabled || s.Disabled
		base.MCPServers[name] = &m
	}
}

func validate(p Profile) error {
	for name, s := range p.MCPServers {
		if s == nil || s.Disabled {
			continue
		}
		if !validName.MatchString(name) {
			return fmt.Errorf("mcpServers: invalid server name %q (it becomes a command name)", name)
		}
		switch s.Type {
		case "", "stdio":
			if s.Command == "" && s.URL == "" {
				return fmt.Errorf("mcpServers.%s: needs a command or a url", name)
			}
		case "http", "streamable-http", "sse":
			if s.URL == "" {
				return fmt.Errorf("mcpServers.%s: type %s needs a url", name, s.Type)
			}
		default:
			return fmt.Errorf("mcpServers.%s: unknown type %q (want stdio, http or sse)", name, s.Type)
		}
		for k, v := range s.Defaults {
			if _, _, err := mcphost.ParseDynamicDefault(v); err != nil {
				return fmt.Errorf("mcpServers.%s.defaults.%s: %w", name, k, err)
			}
		}
		if s.ErrorPattern != "" {
			if _, err := regexp.Compile(s.ErrorPattern); err != nil {
				return fmt.Errorf("mcpServers.%s.errorPattern: %w", name, err)
			}
		}
		if e := s.Exec; e != nil {
			if _, ok := scripts.Langs[e.Lang]; !ok {
				return fmt.Errorf("mcpServers.%s.exec: unknown lang %q (want python or luau)", name, e.Lang)
			}
			if e.Tool == "" || e.Param == "" {
				return fmt.Errorf("mcpServers.%s.exec: needs tool and param", name)
			}
			if _, err := regexp.Compile(e.ErrorTrim); err != nil {
				return fmt.Errorf("mcpServers.%s.exec.errorTrim: %w", name, err)
			}
		}
	}
	return nil
}

var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandVars replaces ${VAR} and ${VAR:-default}, as .mcp.json does.
func ExpandVars(s string) string {
	return varRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := varRef.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(sub[1]); ok && v != "" {
			return v
		}
		return sub[2]
	})
}

func expand(p Profile) Profile {
	out := p
	if p.Host != nil {
		h := *p.Host
		h.Address, h.Token = ExpandVars(h.Address), ExpandVars(h.Token)
		out.Host = &h
	}
	out.Root = ExpandVars(p.Root)
	if p.MCPServers != nil {
		out.MCPServers = make(map[string]*Server, len(p.MCPServers))
		for name, s := range p.MCPServers {
			if s == nil {
				continue
			}
			c := *s
			c.Command, c.URL = ExpandVars(s.Command), ExpandVars(s.URL)
			c.Args = make([]string, len(s.Args))
			for i, a := range s.Args {
				c.Args[i] = ExpandVars(a)
			}
			c.Env = expandMap(s.Env)
			c.Headers = expandMap(s.Headers)
			out.MCPServers[name] = &c
		}
	}
	return out
}

func expandMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = ExpandVars(v)
	}
	return out
}

// ServerNames returns the enabled upstream servers in a stable order.
func (p Profile) ServerNames() []string {
	var names []string
	for name, s := range p.MCPServers {
		if s != nil && !s.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ErrNoSources is returned when a config defines nothing to connect to.
var ErrNoSources = errors.New("the config has no host and no enabled mcpServers")
