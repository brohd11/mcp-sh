package mcphost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/engine"
	"github.com/brohd11/agent-shell/host"
)

// namespace is the command for one server, built per run from its tool list.
type namespace struct {
	server       *Server
	tools        []*mcp.Tool
	instructions string
}

func (n *namespace) find(name string) *mcp.Tool {
	norm := strings.ReplaceAll(name, "-", "_")
	for _, t := range n.tools {
		if t.Name == name || t.Name == norm {
			return t
		}
	}
	return nil
}

func (n *namespace) summary() string {
	names := make([]string, len(n.tools))
	for i, t := range n.tools {
		names[i] = t.Name
	}
	s := fmt.Sprintf("MCP server, %d tools: %s", len(names), strings.Join(names, ", "))
	if len(s) > 110 {
		s = s[:107] + "..."
	}
	return s
}

func (n *namespace) help() string {
	name := n.server.cfg.Name
	var b strings.Builder
	fmt.Fprintf(&b, "usage: %s TOOL [--param value ...] | %s TOOL --json '{...}' | %s TOOL --help\n", name, name, name)
	fmt.Fprintf(&b, "Tools of the MCP server %s:\n", name)
	width := 0
	for _, t := range n.tools {
		width = max(width, len(t.Name))
	}
	for _, t := range n.tools {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, t.Name, firstSentence(toolDescription(t)))
	}
	if n.instructions != "" {
		fmt.Fprintf(&b, "\nServer instructions:\n%s\n", strings.TrimSpace(n.instructions))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (n *namespace) run(ctx context.Context, inv *engine.Invocation) int {
	name := n.server.cfg.Name
	if len(inv.Args) == 0 || inv.Args[0] == "--help" || inv.Args[0] == "-h" || inv.Args[0] == "help" {
		fmt.Fprintln(inv.Stdout, n.help())
		return 0
	}
	tool := n.find(inv.Args[0])
	if tool == nil {
		fmt.Fprintf(inv.Stderr, "%s: no tool named %q (run '%s --help' to list them)\n", name, inv.Args[0], name)
		return 127
	}
	rest := inv.Args[1:]
	if len(rest) == 1 && (rest[0] == "--help" || rest[0] == "-h") {
		fmt.Fprintln(inv.Stdout, ToolHelp(name, tool, n.server.cfg.Defaults))
		return 0
	}
	readStdin := func() (string, error) { return host.ReadStdin(inv, 0) }
	args, err := ParseArgs(tool.InputSchema, rest, readStdin, n.server.cfg.Defaults)
	if err != nil {
		fmt.Fprintf(inv.Stderr, "%s %s: %v\n(run '%s %s --help' for its parameters)\n", name, tool.Name, err, name, tool.Name)
		return 2
	}
	res, err := n.server.CallTool(ctx, tool.Name, args)
	if err != nil {
		fmt.Fprintf(inv.Stderr, "%s %s: %v\n", name, tool.Name, err)
		return 1
	}
	if re := n.server.cfg.ErrorPattern; re != nil && !res.IsError && re.MatchString(ResultText(res)) {
		host.WriteLine(inv.Stderr, ResultText(res))
		return 1
	}
	return WriteResult(inv, res)
}

// ResultText renders a tool result's content: text as-is, other content as short
// placeholders, and structured content as JSON when there is no text.
func ResultText(res *mcp.CallToolResult) string {
	var parts []string
	hasText := false
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, c.Text)
			hasText = true
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *mcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource %s]", c.URI))
		case *mcp.EmbeddedResource:
			if c.Resource != nil && c.Resource.Text != "" {
				parts = append(parts, c.Resource.Text)
				hasText = true
			} else if c.Resource != nil {
				parts = append(parts, fmt.Sprintf("[resource %s, %d bytes]", c.Resource.URI, base64.StdEncoding.DecodedLen(len(c.Resource.Blob))))
			}
		default:
			b, _ := json.Marshal(c)
			parts = append(parts, string(b))
		}
	}
	if !hasText && res.StructuredContent != nil {
		if b, err := json.MarshalIndent(res.StructuredContent, "", "  "); err == nil {
			parts = append(parts, string(b))
		}
	}
	return strings.Join(parts, "\n")
}

// WriteResult writes a tool result to stdout, or to stderr with exit 1 when the tool
// reported an error.
func WriteResult(inv *engine.Invocation, res *mcp.CallToolResult) int {
	text := ResultText(res)
	if res.IsError {
		host.WriteLine(inv.Stderr, text)
		return 1
	}
	host.WriteLine(inv.Stdout, text)
	return 0
}

// ToolHelp renders a tool's description and parameters as command help. Parameters
// with a config default are optional in the usage line and say where the value
// comes from.
func ToolHelp(namespace string, tool *mcp.Tool, defaults map[string]any) string {
	var b strings.Builder
	props := properties(tool.InputSchema)
	req := required(tool.InputSchema)
	names := sortedParams(props, req)

	usage := namespace + " " + tool.Name
	optional := false
	for _, p := range names {
		if _, defaulted := defaults[p]; req[p] && !defaulted {
			usage += " --" + p + " " + strings.ToUpper(paramType(props[p]))
		} else {
			optional = true
		}
	}
	if optional {
		usage += " [options]"
	}
	fmt.Fprintf(&b, "usage: %s\n", usage)
	if desc := toolDescription(tool); desc != "" {
		fmt.Fprintf(&b, "%s\n", strings.TrimSpace(desc))
	}
	if len(names) > 0 {
		b.WriteString("\nParameters:\n")
		width := 0
		for _, p := range names {
			width = max(width, len(p)+len(paramType(props[p]))+3)
		}
		for _, p := range names {
			prop, _ := props[p].(map[string]any)
			label := "--" + p + " " + paramType(prop)
			var notes []string
			if req[p] {
				notes = append(notes, "required")
			}
			if dv, ok := defaults[p]; ok {
				if d, dynamic, _ := ParseDynamicDefault(dv); dynamic && d != nil {
					notes = append(notes, "config fills it from "+d.Tool)
				} else {
					b, _ := json.Marshal(dv)
					notes = append(notes, "config default "+string(b))
				}
			}
			if enum, ok := prop["enum"].([]any); ok {
				notes = append(notes, "one of "+jsonList(enum))
			}
			if def, ok := prop["default"]; ok {
				d, _ := json.Marshal(def)
				notes = append(notes, "default "+string(d))
			}
			line := fmt.Sprintf("  %-*s", width, label)
			if desc, _ := prop["description"].(string); desc != "" {
				line += "  " + firstLine(desc)
			}
			if len(notes) > 0 {
				line += " (" + strings.Join(notes, "; ") + ")"
			}
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	b.WriteString("\nObjects and arrays take JSON (--tags '[\"a\"]'), or repeat an array flag. ")
	b.WriteString("--json '{...}' passes the whole input; --json - reads it from stdin.")
	return b.String()
}

func toolDescription(t *mcp.Tool) string {
	if t.Description != "" {
		return t.Description
	}
	if t.Title != "" {
		return t.Title
	}
	if t.Annotations != nil {
		return t.Annotations.Title
	}
	return ""
}

func sortedParams(props map[string]any, req map[string]bool) []string {
	names := make([]string, 0, len(props))
	for p := range props {
		names = append(names, p)
	}
	sort.Slice(names, func(i, j int) bool {
		if req[names[i]] != req[names[j]] {
			return req[names[i]]
		}
		return names[i] < names[j]
	})
	return names
}

func jsonList(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		b, _ := json.Marshal(v)
		parts[i] = string(b)
	}
	return strings.Join(parts, ", ")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// firstSentence shortens a tool description for listings.
func firstSentence(s string) string {
	s = firstLine(s)
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if len(s) > 100 {
		s = s[:97] + "..."
	}
	return s
}
