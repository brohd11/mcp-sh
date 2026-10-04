package builtins

import (
	"context"
	"fmt"

	"github.com/brohd11/mcp-sh/engine"
)

// Host returns the `host` command, which runs a host command by name even when a
// builtin or a shell keyword of the same name hides it: `host test ...`.
func Host(hostCmds []engine.Command) engine.Command {
	byName := make(map[string]engine.Command, len(hostCmds))
	for _, c := range hostCmds {
		byName[c.Name] = c
	}
	const help = "usage: host COMMAND [ARGS...]\nRun the host's COMMAND even when a shell builtin of the same name (test, read, type, ...) hides it."
	return engine.Command{
		Name:    "host",
		Summary: "run a host command whose name a shell builtin hides: host NAME [ARGS...]",
		Help:    help,
		Source:  "builtin",
		Run: func(ctx context.Context, inv *engine.Invocation) int {
			if len(inv.Args) == 0 {
				fmt.Fprintln(inv.Stderr, help)
				return 2
			}
			cmd, ok := byName[inv.Args[0]]
			if !ok {
				fmt.Fprintf(inv.Stderr, "host: the host has no command named %q\n", inv.Args[0])
				return 127
			}
			sub := *inv
			sub.Args = inv.Args[1:]
			return cmd.Run(ctx, &sub)
		},
	}
}
