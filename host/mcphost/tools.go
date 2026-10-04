package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	text, err := n.server.renderResult(tool.Name, res)
	if err != nil {
		fmt.Fprintf(inv.Stderr, "%s %s: %v\n", name, tool.Name, err)
		return 1
	}
	return writeText(inv, text, res.IsError)
}

// ResultText renders a tool result's content: text as-is, other content as short
// placeholders, and structured content as JSON when there is no text.
func ResultText(res *mcp.CallToolResult) string {
	text, _ := resultText(res, nil)
	return text
}

// resultText is ResultText with images handed to saveImage, whose return value
// replaces the placeholder. A nil saveImage keeps the placeholder.
func resultText(res *mcp.CallToolResult, saveImage func(mimeType string, data []byte) (string, error)) (string, error) {
	var parts []string
	hasText := false
	image := func(mimeType string, data []byte, placeholder string) error {
		if saveImage == nil {
			parts = append(parts, placeholder)
			return nil
		}
		path, err := saveImage(mimeType, data)
		if err != nil {
			return err
		}
		parts = append(parts, path)
		return nil
	}
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, c.Text)
			hasText = true
		case *mcp.ImageContent:
			if err := image(c.MIMEType, c.Data, fmt.Sprintf("[image %s, %d bytes]", c.MIMEType, len(c.Data))); err != nil {
				return "", err
			}
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *mcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource %s]", c.URI))
		case *mcp.EmbeddedResource:
			switch r := c.Resource; {
			case r == nil:
			case r.Text != "":
				parts = append(parts, r.Text)
				hasText = true
			default:
				placeholder := fmt.Sprintf("[resource %s, %d bytes]", r.URI, len(r.Blob))
				if !strings.HasPrefix(r.MIMEType, "image/") {
					parts = append(parts, placeholder)
				} else if err := image(r.MIMEType, r.Blob, placeholder); err != nil {
					return "", err
				}
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
	return strings.Join(parts, "\n"), nil
}

// renderResult is ResultText for output the agent reads: with an ImageDir, images are
// saved there and shown as their path on a line of its own, so the agent can open them.
func (s *Server) renderResult(tool string, res *mcp.CallToolResult) (string, error) {
	if s.cfg.ImageDir == "" {
		return ResultText(res), nil
	}
	n := 0
	return resultText(res, func(mimeType string, data []byte) (string, error) {
		n++
		return saveImage(s.cfg.ImageDir, fmt.Sprintf("%s-%s", s.cfg.Name, tool), n, mimeType, data)
	})
}

// keepImages is how many saved images an image folder keeps; older ones are removed.
const keepImages = 50

var imageExts = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/jpg": ".jpg", "image/gif": ".gif",
	"image/webp": ".webp", "image/svg+xml": ".svg", "image/bmp": ".bmp", "image/tiff": ".tiff",
}

// saveImage writes one image to dir and returns its absolute path, then prunes dir to
// the newest keepImages files.
func saveImage(dir, prefix string, n int, mimeType string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("saving image: %w", err)
	}
	ext, ok := imageExts[strings.ToLower(mimeType)]
	if !ok {
		ext = ".bin"
	}
	name := fmt.Sprintf("%s-%s-%d%s", prefix, time.Now().Format("20060102-150405.000"), n, ext)
	path, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("saving image: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("saving image: %w", err)
	}
	pruneImages(dir, keepImages)
	return path, nil
}

// pruneImages removes all but the newest keep files in dir. Failures are ignored: a
// full folder is not worth failing a tool call over.
func pruneImages(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(files) <= keep {
		return
	}
	// Newest first; names carry the time, so they break ties within a timestamp.
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].path > files[j].path
	})
	for _, f := range files[keep:] {
		os.Remove(f.path)
	}
}

// WriteResult writes a tool result to stdout, or to stderr with exit 1 when the tool
// reported an error.
func WriteResult(inv *engine.Invocation, res *mcp.CallToolResult) int {
	return writeText(inv, ResultText(res), res.IsError)
}

func writeText(inv *engine.Invocation, text string, isError bool) int {
	if isError {
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
