package builtins

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/brohd11/agent-shell/engine"
)

// Help returns the `help` command over reg: `help` lists every command, `help NAME`
// shows one command's help.
func Help(reg engine.Registry) engine.Command {
	return engine.Command{
		Name:    "help",
		Summary: "list commands, or show help for one: help [command]",
		Help:    "usage: help [command]\nWith no argument, list every command. With one, show its help.",
		Source:  "builtin",
		Run: func(ctx context.Context, inv *engine.Invocation) int {
			switch len(inv.Args) {
			case 0:
				io.WriteString(inv.Stdout, FormatList(reg))
				return 0
			case 1:
				text, ok := CommandHelp(ctx, reg, inv.Args[0])
				if !ok {
					fmt.Fprintf(inv.Stderr, "help: no command named %q (shell builtins such as echo, printf, test and read behave as in bash)\n", inv.Args[0])
					return 1
				}
				fmt.Fprintln(inv.Stdout, strings.TrimRight(text, "\n"))
				return 0
			default:
				fmt.Fprintln(inv.Stderr, "usage: help [command]")
				return 2
			}
		},
	}
}

// CommandHelp returns the help text for name: its Help, else HelpFunc's answer, else
// its summary. It never runs the command itself.
func CommandHelp(ctx context.Context, reg engine.Registry, name string) (string, bool) {
	cmd, ok := reg.Lookup(name)
	if !ok {
		return "", false
	}
	if cmd.Help != "" {
		return cmd.Help, true
	}
	if cmd.HelpFunc != nil {
		if text, err := cmd.HelpFunc(ctx); err == nil && strings.TrimSpace(text) != "" {
			return text, true
		}
	}
	if cmd.Summary != "" {
		return name + ": " + cmd.Summary, true
	}
	return name + ": no help available", true
}

// FormatList renders the command list, host commands first.
func FormatList(reg engine.Registry) string {
	groups := map[string][]engine.Command{}
	var order []string
	for _, c := range reg.Sorted() {
		src := c.Source
		if src == "" {
			src = "other"
		}
		if _, seen := groups[src]; !seen {
			order = append(order, src)
		}
		groups[src] = append(groups[src], c)
	}
	rank := func(s string) int {
		switch s {
		case "host":
			return 0
		case "builtin":
			return 2
		}
		return 1
	}
	sort.SliceStable(order, func(i, j int) bool { return rank(order[i]) < rank(order[j]) })
	titles := map[string]string{
		"host":    "Host commands",
		"mcp":     "MCP servers (tools are subcommands: NAME TOOL --help)",
		"script":  "Script commands",
		"builtin": "Builtins (in-process)",
	}

	var b strings.Builder
	for gi, src := range order {
		if gi > 0 {
			b.WriteString("\n")
		}
		title := titles[src]
		if title == "" {
			title = strings.ToUpper(src[:1]) + src[1:] + " commands"
		}
		fmt.Fprintf(&b, "%s:\n", title)
		width := 0
		for _, c := range groups[src] {
			width = max(width, len(c.Name))
		}
		for _, c := range groups[src] {
			summary := firstLine(c.Summary)
			if c.Replaces != nil && c.Replaces.Source == "builtin" {
				summary = strings.TrimSpace(summary + " (replaces the builtin " + c.Name + ")")
			}
			if summary == "" {
				fmt.Fprintf(&b, "  %s\n", c.Name)
			} else {
				fmt.Fprintf(&b, "  %-*s  %s\n", width, c.Name, summary)
			}
		}
	}
	return b.String()
}
