// Package scripts turns script files into shell commands. Each file is run through an
// upstream MCP server's code-execution tool (blender-mcp's execute_blender_code, Roblox
// Studio's execute_luau, ...), with the command's arguments and stdin injected as variables:
//
//	# summary: list scene objects as JSON lines
//	# Prints one {"name", "type"} object per line.
//	import bpy, json
//	for o in bpy.data.objects:
//	    print(json.dumps({"name": o.name, "type": o.type}))
//
// The leading comment block is the command's help; a "summary:" line is its one-line
// summary and a "server:" line picks the server when several run the same language.
// Files are re-read on every run, so edits apply immediately.
//
// A file named _lib<ext> (e.g. _lib.luau) is not a command: it is shared code every
// script of that language gets, as a `lib` table in Luau. Libraries from all folders
// are concatenated in folder order, so a user folder can add helpers.
package scripts

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/engine"
	"github.com/brohd11/agent-shell/host"
	"github.com/brohd11/agent-shell/host/mcphost"
)

// Lang knows how to recognize and prepare scripts in one language.
type Lang struct {
	Ext     string // ".py"
	Comment string // line comment marker, "#"
	// Wrap turns a script body into the code sent to the server, declaring ARGS (list
	// of strings), STDIN and CWD (strings) first, plus the shared library.
	Wrap func(args []string, stdin, cwd, body, lib string) string
}

var Langs = map[string]Lang{
	"python": {Ext: ".py", Comment: "#", Wrap: pythonWrap},
	"luau":   {Ext: ".luau", Comment: "--", Wrap: luauWrap},
}

// Caller is the part of an upstream server a script needs.
type Caller interface {
	Name() string
	CallTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)
}

// Binding routes scripts of one language to a server's code tool.
type Binding struct {
	Server Caller
	Tool   string // e.g. "execute_blender_code"
	Param  string // the tool's code argument, e.g. "code"
	Lang   string // key of Langs
	// OutputPrefix is stripped from successful output ("Code executed successfully: ").
	OutputPrefix string
	// ErrorPrefix marks output that is really an error, for servers that report
	// exceptions as normal text ("Error executing code: ").
	ErrorPrefix string
	// ErrorTrim is removed from error text (e.g. Studio's internal "file:line: "
	// location prefixes).
	ErrorTrim *regexp.Regexp
}

// Dir is one folder of scripts. Later dirs override earlier ones by command name.
type Dir struct {
	Label string // shown in help, e.g. "~/.agent-shell/blender/commands"
	FS    fs.FS
	// Required marks a folder the user named explicitly (commandDirs): a missing one is
	// reported in the source label instead of being skipped silently.
	Required bool
}

type Source struct {
	Dirs     []Dir
	Bindings []Binding
}

var _ host.Source = (*Source)(nil)

func (s *Source) Label() string {
	var missing []string
	for _, d := range s.Dirs {
		if _, err := fs.Stat(d.FS, "."); d.Required && err != nil {
			missing = append(missing, d.Label)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("script commands (%d folders, missing: %s)", len(s.Dirs), strings.Join(missing, ", "))
	}
	return fmt.Sprintf("script commands (%d folders)", len(s.Dirs))
}

type script struct {
	name, file string
	dir        Dir
	lang       Lang
	header     header
	overrides  *Dir // the earlier folder whose script of the same name this one replaces
}

type header struct {
	summary, server, help string
}

// Commands scans the folders. A missing folder is not an error: users create them on
// demand.
func (s *Source) Commands(ctx context.Context) ([]engine.Command, error) {
	byExt := map[string]Lang{}
	for _, b := range s.Bindings {
		if l, ok := Langs[b.Lang]; ok {
			byExt[l.Ext] = l
		}
	}
	found := map[string]script{}
	for _, d := range s.Dirs {
		entries, err := fs.ReadDir(d.FS, ".")
		if err != nil {
			continue
		}
		for _, e := range entries {
			lang, ok := byExt[path.Ext(e.Name())]
			if e.IsDir() || !ok || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
				continue
			}
			body, err := fs.ReadFile(d.FS, e.Name())
			if err != nil {
				continue
			}
			name := strings.TrimSuffix(e.Name(), lang.Ext)
			sc := script{name: name, file: e.Name(), dir: d, lang: lang, header: parseHeader(string(body), lang.Comment)}
			if prev, ok := found[name]; ok {
				sc.overrides = &prev.dir
			}
			found[name] = sc
		}
	}
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	cmds := make([]engine.Command, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, s.command(found[n]))
	}
	return cmds, nil
}

func (s *Source) binding(sc script) (Binding, error) {
	var matches []Binding
	for _, b := range s.Bindings {
		if Langs[b.Lang].Ext != sc.lang.Ext {
			continue
		}
		if sc.header.server != "" && b.Server.Name() != sc.header.server {
			continue
		}
		matches = append(matches, b)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Binding{}, fmt.Errorf("no server named %q runs %s scripts", sc.header.server, sc.lang.Ext)
	}
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.Server.Name()
	}
	return Binding{}, fmt.Errorf("several servers run %s scripts (%s); add a '%s server: NAME' line", sc.lang.Ext, strings.Join(names, ", "), sc.lang.Comment)
}

func (s *Source) command(sc script) engine.Command {
	b, bindErr := s.binding(sc)
	via := ""
	if bindErr == nil {
		via = fmt.Sprintf("runs via %s %s", b.Server.Name(), b.Tool)
	}
	summary := sc.header.summary
	if summary == "" {
		summary = "script command"
	}
	help := fmt.Sprintf("usage: %s [ARGS...]\n", sc.name)
	if sc.header.help != "" {
		help += sc.header.help + "\n"
	} else {
		help += summary + "\n"
	}
	where := fmt.Sprintf("script %s in %s", sc.file, sc.dir.Label)
	if sc.overrides != nil {
		note := "overrides the one in " + sc.overrides.Label
		summary += " (" + note + ")"
		where += "; " + note
	}
	help += fmt.Sprintf("(%s; %s)", where, via)
	return engine.Command{
		Name:    sc.name,
		Summary: summary,
		Help:    help,
		Source:  "script",
		Run: func(ctx context.Context, inv *engine.Invocation) int {
			if bindErr != nil {
				fmt.Fprintf(inv.Stderr, "%s: %v\n", sc.name, bindErr)
				return 1
			}
			stdin, err := host.ReadStdin(inv, 0)
			if err != nil {
				fmt.Fprintf(inv.Stderr, "%s: %v\n", sc.name, err)
				return 1
			}
			// Re-read so an edit made since the listing still applies.
			body, err := fs.ReadFile(sc.dir.FS, sc.file)
			if err != nil {
				fmt.Fprintf(inv.Stderr, "%s: %v\n", sc.name, err)
				return 1
			}
			code := sc.lang.Wrap(inv.Args, stdin, inv.Dir, string(body), s.library(sc.lang.Ext))
			res, err := b.Server.CallTool(ctx, b.Tool, map[string]any{b.Param: code})
			if err != nil {
				fmt.Fprintf(inv.Stderr, "%s: %v\n", sc.name, err)
				return 1
			}
			text := mcphost.ResultText(res)
			if res.IsError || (b.ErrorPrefix != "" && strings.HasPrefix(text, b.ErrorPrefix)) {
				text = strings.TrimPrefix(text, b.ErrorPrefix)
				if b.ErrorTrim != nil {
					text = b.ErrorTrim.ReplaceAllString(text, "")
				}
				host.WriteLine(inv.Stderr, text)
				return 1
			}
			host.WriteLine(inv.Stdout, strings.TrimPrefix(text, b.OutputPrefix))
			return 0
		},
	}
}

// library concatenates the _lib<ext> files of every folder, in folder order.
func (s *Source) library(ext string) string {
	var parts []string
	for _, d := range s.Dirs {
		if b, err := fs.ReadFile(d.FS, "_lib"+ext); err == nil {
			parts = append(parts, string(b))
		}
	}
	return strings.Join(parts, "\n")
}

// parseHeader reads the leading comment block (after an optional shebang).
func parseHeader(body, marker string) header {
	var h header
	var help []string
	for i, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if i == 0 && strings.HasPrefix(trimmed, "#!") {
			continue
		}
		if !strings.HasPrefix(trimmed, marker) {
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(trimmed, marker))
		key, val, _ := strings.Cut(text, ":")
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "summary":
			h.summary = strings.TrimSpace(val)
			continue
		case "server":
			h.server = strings.TrimSpace(val)
			continue
		}
		help = append(help, text)
	}
	h.help = strings.TrimSpace(strings.Join(help, "\n"))
	return h
}

// pythonWrap prepends the variables on one line, so error line numbers are the
// script's plus one. Python code servers (blender-mcp) capture stdout themselves.
// A library is defined first and shifts error line numbers by its length.
func pythonWrap(args []string, stdin, cwd, body, lib string) string {
	if args == nil {
		args = []string{}
	}
	// JSON string and string-list literals are valid Python literals.
	a, _ := json.Marshal(args)
	in, _ := json.Marshal(stdin)
	c, _ := json.Marshal(cwd)
	if lib != "" {
		body = lib + "\n" + body
	}
	return fmt.Sprintf("ARGS = %s; STDIN = %s; CWD = %s\n%s", a, in, c, body)
}

// luauWrap runs the body as a function and returns everything it printed, plus its
// return value. Luau code tools (Roblox Studio's execute_luau) return only the chunk's
// return value; print output would otherwise go to Studio's log, not to the shell.
// The prelude shares the body's first line, so error line numbers match the file.
// The body becomes a function that runs after the library has filled `lib`, which is
// why the library can come after it.
func luauWrap(args []string, stdin, cwd, body, lib string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = luaQuote(a)
	}
	prelude := fmt.Sprintf(`local ARGS = {%s}; local STDIN = %s; local CWD = %s; local lib = {}; `+
		`local __agent_shell_out = {}; `+
		`local function print(...) local parts = table.pack(...); for i = 1, parts.n do parts[i] = tostring(parts[i]) end; table.insert(__agent_shell_out, table.concat(parts, "\t")) end; `+
		`local __agent_shell_main = function() `, strings.Join(quoted, ", "), luaQuote(stdin), luaQuote(cwd))
	return prelude + body + `
end
do
` + lib + `
end
local __agent_shell_ret = __agent_shell_main()
if __agent_shell_ret ~= nil then table.insert(__agent_shell_out, tostring(__agent_shell_ret)) end
return table.concat(__agent_shell_out, "\n")`
}

// luaQuote writes a Lua/Luau string literal: escapes for quotes, backslashes and
// control bytes; other bytes (UTF-8 included) pass through.
func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\%03d`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
