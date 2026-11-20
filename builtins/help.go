package builtins

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/brohd11/mcp-sh/engine"
)

// ListCap is the number of commands above which FormatList prints names only, so a
// host with hundreds of commands does not flood the agent's context.
const ListCap = 150

// Help returns the `help` command over reg: `help` lists every command, `help -s TERM`
// lists the commands matching TERM, `help NAME` shows one command's help.
func Help(reg engine.Registry) engine.Command {
	return engine.Command{
		Name:    "help",
		Summary: "list commands, search them, or show help for one: help [-s TERM | command]",
		Help: "usage: help [-s TERM | command]\n" +
			"With no argument, list every command (names only when there are more than " + fmt.Sprint(ListCap) + ").\n" +
			"With -s TERM (or --search), list the commands whose name or summary contains every word of TERM, ignoring case.\n" +
			"With a command name, show its help.",
		Source: "builtin",
		Run: func(ctx context.Context, inv *engine.Invocation) int {
			if len(inv.Args) > 0 && (inv.Args[0] == "-s" || inv.Args[0] == "--search") {
				term := strings.Join(inv.Args[1:], " ")
				if strings.TrimSpace(term) == "" {
					fmt.Fprintln(inv.Stderr, "usage: help -s TERM")
					return 2
				}
				list := SearchList(reg, term)
				if list == "" {
					fmt.Fprintf(inv.Stderr, "help: no commands match %q\n", term)
					return 1
				}
				io.WriteString(inv.Stdout, list)
				return 0
			}
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
				fmt.Fprintln(inv.Stderr, "usage: help [-s TERM | command]")
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

// FormatList renders the command list, host commands first. Above ListCap commands it
// prints names only, grouped, and ends with a hint to search instead.
func FormatList(reg engine.Registry) string {
	cmds := reg.Sorted()
	if len(cmds) <= ListCap {
		return formatGroups(cmds, false)
	}
	return formatGroups(cmds, true) + fmt.Sprintf(
		"\n%d commands, names only. Search names and summaries with list_commands' filter or `help -s TERM`; "+
			"show one command's help with `help NAME`.\n", len(cmds))
}

// SearchList renders, with summaries, the commands whose name or summary contains every
// whitespace-separated word of term, ignoring case. It returns "" when none match.
func SearchList(reg engine.Registry, term string) string {
	words := strings.Fields(strings.ToLower(term))
	var matched []engine.Command
	for _, c := range reg.Sorted() {
		hay := strings.ToLower(c.Name + " " + c.Summary)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		return ""
	}
	return formatGroups(matched, false)
}

// formatGroups renders cmds grouped by source, host commands first and builtins last.
// compact prints the names only, wrapped, with a count per group.
func formatGroups(cmds []engine.Command, compact bool) string {
	groups := map[string][]engine.Command{}
	var order []string
	for _, c := range cmds {
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
		if compact {
			fmt.Fprintf(&b, "%s (%d):\n", title, len(groups[src]))
			writeWrapped(&b, groups[src], 100)
			continue
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

// writeWrapped writes the command names indented two spaces, space-separated, in lines
// of at most width columns (a longer name gets a line of its own).
func writeWrapped(b *strings.Builder, cmds []engine.Command, width int) {
	line := 0
	for _, c := range cmds {
		if line > 0 && line+1+len(c.Name) > width {
			b.WriteString("\n")
			line = 0
		}
		if line == 0 {
			b.WriteString("  ")
			line = 2
		} else {
			b.WriteString(" ")
			line++
		}
		b.WriteString(c.Name)
		line += len(c.Name)
	}
	if line > 0 {
		b.WriteString("\n")
	}
}
