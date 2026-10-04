// Package mcpserver exposes a shell.Shell as MCP tools: run, list_commands and help.
package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/builtins"
	"github.com/brohd11/agent-shell/shell"
)

type Options struct {
	Name    string // MCP server name, e.g. "agent-shell-godot"
	Version string
	// Title names the host for the agent, e.g. "the live Godot editor".
	Title string
	// Instructions is host-specific guidance appended to the generic instructions.
	Instructions string
	// FileAccess describes the shell's own file access (redirects, source, globs, file
	// operands of builtins) for the agent, e.g. "disabled".
	FileAccess string
	// MaxTimeout caps the per-call timeout_seconds an agent may request.
	MaxTimeout time.Duration
}

type runInput struct {
	Script         string `json:"script" jsonschema:"bash script to run: commands, pipes, &&, ||, ;, if/for/while, functions, variables, $(...)"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"optional timeout for this call, in seconds"`
}

type helpInput struct {
	Command string `json:"command" jsonschema:"command name, as listed by list_commands"`
}

// New builds the MCP server. Serve it with server.Run(ctx, &mcp.StdioTransport{}).
func New(sh *shell.Shell, opts Options) *mcp.Server {
	if opts.FileAccess == "" {
		opts.FileAccess = "disabled"
	}
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = 10 * time.Minute
	}
	server := mcp.NewServer(&mcp.Implementation{Name: opts.Name, Version: opts.Version},
		&mcp.ServerOptions{Instructions: instructions(opts)})

	mcp.AddTool(server, &mcp.Tool{
		Name: "run",
		Description: fmt.Sprintf("Run a bash script in a sandboxed shell connected to %s, and return its output. "+
			"Commands are the host's commands plus in-process builtins (grep, head, tail, wc, sort, uniq, cut, tr, sed s///, jq, seq, cat); "+
			"no external programs or network. Shell file access (redirects, globs, builtin file operands): %s; "+
			"host commands act with the host's own permissions. "+
			"Call list_commands first to discover the host's commands. "+
			"Example: `help` or `<command> --help | head -20`.", opts.Title, opts.FileAccess),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, any, error) {
		timeout := time.Duration(in.TimeoutSeconds) * time.Second
		if timeout > opts.MaxTimeout {
			timeout = opts.MaxTimeout
		}
		res := sh.Run(ctx, in.Script, timeout)
		return textResult(res.Text(), res.Err != nil || res.ExitCode != 0), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_commands",
		Description: fmt.Sprintf("List every command available in the %s shell (host commands and builtins) with a one-line summary.", opts.Title),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		reg, statuses, err := sh.Commands(ctx)
		if err != nil {
			return textResult(err.Error(), true), nil, nil
		}
		return textResult(shell.FormatSources(statuses)+"\n"+builtins.FormatList(reg), false), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "help",
		Description: "Show the full help (usage, flags, subcommands) for one command.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in helpInput) (*mcp.CallToolResult, any, error) {
		reg, _, err := sh.Commands(ctx)
		if err != nil {
			return textResult(err.Error(), true), nil, nil
		}
		text, ok := builtins.CommandHelp(ctx, reg, in.Command)
		if !ok {
			return textResult(fmt.Sprintf("no command named %q; call list_commands", in.Command), true), nil, nil
		}
		return textResult(text, false), nil, nil
	})

	return server
}

func instructions(opts Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This server is a sandboxed bash shell connected to %s. ", opts.Title)
	b.WriteString("Use the run tool for everything: compose host commands with pipes, loops and the builtin text tools " +
		"(grep, head, jq, ...) instead of making many separate calls. " +
		"Only listed commands exist; there are no external programs. " +
		"Shell file access (redirects, globs, builtin file operands): " + opts.FileAccess + ". " +
		"Host commands act with the host's own permissions.")
	if opts.Instructions != "" {
		b.WriteString("\n\n")
		b.WriteString(opts.Instructions)
	}
	return b.String()
}

func textResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: isError,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}
