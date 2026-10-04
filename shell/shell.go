// Package shell ties command sources (native host, upstream MCP servers, scripts), the
// builtins and an engine together into one runnable shell. Both the MCP server and the
// CLI's `run` go through Shell.Run.
package shell

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/brohd11/mcp-sh/builtins"
	"github.com/brohd11/mcp-sh/engine"
	"github.com/brohd11/mcp-sh/host"
)

const (
	DefaultTimeout   = 120 * time.Second
	DefaultMaxOutput = 64 << 10
)

type Shell struct {
	// Sources supply the shell's commands. On a name clash a later source wins over an
	// earlier one, and every source wins over the generic builtins.
	Sources []host.Source
	Engine  engine.Engine
	// Extra are Go-side commands added by the embedding binary. They take precedence
	// over the builtins and every source.
	Extra     []engine.Command
	Timeout   time.Duration // default DefaultTimeout
	MaxOutput int           // per stream, in bytes; default DefaultMaxOutput
}

// Result is the outcome of one script run.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
	// Err is set when the script could not run to completion: no source reachable,
	// parse error, timeout. ExitCode is still meaningful (124 for timeouts).
	Err error
	// Warnings report sources that were unavailable while others worked.
	Warnings []string
}

// SourceStatus is one source's state after fetching commands.
type SourceStatus struct {
	Label    string
	Commands int
	Err      error
}

// Commands fetches every source's commands and returns the full registry for a run.
// A failing source only produces a SourceStatus error, unless every source failed.
func (s *Shell) Commands(ctx context.Context) (engine.Registry, []SourceStatus, error) {
	var cmds []engine.Command
	var statuses []SourceStatus
	var errs []string
	for _, src := range s.Sources {
		got, err := src.Commands(ctx)
		cmds = append(cmds, got...)
		statuses = append(statuses, SourceStatus{Label: src.Label(), Commands: len(got), Err: err})
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(s.Sources) > 0 && len(errs) == len(s.Sources) {
		return nil, statuses, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	if r, ok := s.Engine.(engine.Reserver); ok {
		for i, c := range cmds {
			if r.Reserved(c.Name) {
				cmds[i].Summary = strings.TrimSpace(c.Summary + " (the shell's own " + c.Name + " hides it; run as: host " + c.Name + " ...)")
			}
		}
	}
	return builtins.Registry(cmds, s.Extra), statuses, nil
}

// Warnings formats the failed sources in statuses.
func Warnings(statuses []SourceStatus) []string {
	var out []string
	for _, st := range statuses {
		if st.Err != nil {
			out = append(out, fmt.Sprintf("%s unavailable: %v", st.Label, st.Err))
		}
	}
	return out
}

// FormatSources renders a "Sources:" block for listings.
func FormatSources(statuses []SourceStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Sources:\n")
	for _, st := range statuses {
		if st.Err != nil {
			fmt.Fprintf(&b, "  %s: unavailable: %s\n", st.Label, firstLine(st.Err.Error()))
		} else {
			fmt.Fprintf(&b, "  %s: %d commands\n", st.Label, st.Commands)
		}
	}
	return b.String()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Close releases every source that holds resources (upstream subprocesses, sessions).
func (s *Shell) Close() {
	for _, src := range s.Sources {
		if c, ok := src.(host.Closer); ok {
			c.Close()
		}
	}
}

// Run runs script with the default timeout, or timeout when it is positive.
func (s *Shell) Run(ctx context.Context, script string, timeout time.Duration) Result {
	if timeout <= 0 {
		timeout = s.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	reg, statuses, err := s.Commands(ctx)
	if err != nil {
		return Result{ExitCode: 1, Err: err}
	}
	limit := s.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	stdout := &limitedBuffer{max: limit}
	stderr := &limitedBuffer{max: limit}
	code, err := s.Engine.Run(ctx, script, reg, engine.IO{Stdout: stdout, Stderr: stderr})
	if err != nil && code == 124 {
		err = fmt.Errorf("%w after %s", err, timeout)
	}
	return Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  code,
		Truncated: stdout.truncated || stderr.truncated,
		Err:       err,
		Warnings:  Warnings(statuses),
	}
}

// Text renders a result for an agent: stdout, then stderr and status only when they
// carry information.
func (r Result) Text() string {
	var b strings.Builder
	b.WriteString(r.Stdout)
	section := func(s string) {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	if r.Stderr != "" {
		section("[stderr]\n" + r.Stderr)
	}
	for _, w := range r.Warnings {
		section("[warning] " + w + "\n")
	}
	if r.Truncated {
		section("[output truncated; narrow it with grep, head or jq]\n")
	}
	if r.Err != nil {
		section("[error] " + r.Err.Error() + "\n")
	}
	if r.ExitCode != 0 {
		section(fmt.Sprintf("[exit_code=%d]\n", r.ExitCode))
	}
	if b.Len() == 0 {
		return "(no output, exit_code=0)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// limitedBuffer keeps the first max bytes and silently drops the rest, so a chatty
// command cannot flood the agent's context or fail on a write error. Safe for the
// concurrent writes of pipeline stages.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       strings.Builder
	max       int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	room := l.max - l.buf.Len()
	if room <= 0 {
		l.truncated = l.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		l.buf.Write(p[:room])
		l.truncated = true
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
