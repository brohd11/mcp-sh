// Package engine defines the seam between agent-shell and the interpreter that runs
// scripts. Commands (host commands and in-process builtins) are plain values the engine
// dispatches to; the engine itself decides syntax, control flow, and sandboxing.
package engine

import (
	"context"
	"io"
	"io/fs"
	"sort"
)

// Engine runs one script to completion and returns its exit status. cmds is the full
// set of commands the script may call; anything else must fail as "command not found".
// A non-nil error means the script could not run (syntax error, rejected construct,
// timeout), not that it exited non-zero.
type Engine interface {
	Run(ctx context.Context, script string, cmds Registry, stdio IO) (exitCode int, err error)
}

// Reserver is implemented by engines whose own builtins and keywords take precedence
// over registry commands of the same name (bash's `test`, `read`, `type`, ...).
type Reserver interface {
	Reserved(name string) bool
}

// IO is the standard streams for one script run. Nil Stdin reads as empty, and nil
// Stdout/Stderr discard.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Command is anything the shell can call by name: a host command forwarded to the
// program, or a builtin implemented in Go.
type Command struct {
	Name    string
	Summary string // one line, shown by list_commands
	Help    string // full help, shown by `help <name>`; falls back to HelpFunc, then Summary
	// HelpFunc fetches help lazily when Help is empty (e.g. from the host).
	HelpFunc func(ctx context.Context) (string, error)
	Source   string // where it comes from, e.g. "host" or "builtin"
	Run      func(ctx context.Context, inv *Invocation) int
	// Replaces is set by Registry.Add when this command took over a name from a
	// command of another source (e.g. a host command named like a builtin).
	Replaces *Command
}

// Invocation is one call of a Command.
type Invocation struct {
	Args   []string // arguments, without the command name
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Dir    string // shell's current directory, in the engine's virtual namespace
	// FS is the sandboxed filesystem the script may touch, or nil when file access is
	// disabled. Paths are relative to its root (fs.ValidPath form).
	FS fs.FS
	// Resolve turns a shell path (absolute or relative to Dir) into an FS path.
	Resolve func(path string) (string, error)
}

// Registry is a name-indexed command set. Later registrations win, so a host can
// override a builtin by registering the same name.
type Registry map[string]Command

func (r Registry) Add(cmds ...Command) {
	for _, c := range cmds {
		if prev, ok := r[c.Name]; ok && prev.Source != c.Source {
			c.Replaces = &prev
		}
		r[c.Name] = c
	}
}

func (r Registry) Lookup(name string) (Command, bool) {
	c, ok := r[name]
	return c, ok
}

// Sorted returns the commands ordered by name.
func (r Registry) Sorted() []Command {
	out := make([]Command, 0, len(r))
	for _, c := range r {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
