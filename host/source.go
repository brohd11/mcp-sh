package host

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/brohd11/mcp-sh/engine"
)

// Source supplies shell commands: the native TCP host, an upstream MCP server, a folder
// of script commands. Commands is called at the start of every run, so a source can
// pick up changes (a host loading new commands, an edited script) between runs.
type Source interface {
	// Label names the source in warnings and listings, e.g. "godot 4.7 (127.0.0.1:9510)".
	Label() string
	Commands(ctx context.Context) ([]engine.Command, error)
}

// Closer is implemented by sources holding resources, such as an upstream subprocess.
type Closer interface {
	Close() error
}

// ReadStdin reads a command's piped input, up to limit bytes (DefaultMaxStdin when
// limit <= 0). Commands that forward stdin call this before any lock or round trip, so
// an upstream command in the same pipeline can finish first.
func ReadStdin(inv *engine.Invocation, limit int64) (string, error) {
	if limit <= 0 {
		limit = DefaultMaxStdin
	}
	b, err := io.ReadAll(io.LimitReader(inv.Stdin, limit+1))
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	if int64(len(b)) > limit {
		return "", fmt.Errorf("stdin exceeds %d bytes", limit)
	}
	return string(b), nil
}

// WriteLine writes s, adding the trailing newline hosts often omit so `wc -l`, `;` and
// `$(...)` behave as they would with a Unix tool.
func WriteLine(w io.Writer, s string) {
	if s == "" {
		return
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	io.WriteString(w, s)
}
