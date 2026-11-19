package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/brohd11/mcp-sh/engine"
)

// DefaultMaxStdin caps how much piped input is forwarded to one host invoke.
const DefaultMaxStdin = 16 << 20

// DefaultMaxOutput is the output a host is asked to stop at (max_output) for one invoke.
const DefaultMaxOutput = 4 << 20

// maxResponse caps how much of one response line is read: JSON escaping can grow the
// output several times, and a host that ignores max_output mustn't exhaust memory.
const maxResponse = 64 << 20

// Client talks to one host. Calls are serialized: hosts like Godot handle requests on a
// single thread, and the shell runs pipeline stages concurrently.
type Client struct {
	Addr  string
	Token string
	// Hint is appended to connection errors, e.g. how to start the host's bridge.
	Hint string
	// DialTimeout bounds connecting; the request itself is bounded by the context.
	DialTimeout time.Duration
	// MaxStdin caps forwarded stdin per invoke (default DefaultMaxStdin).
	MaxStdin int64
	// Timeout limits each host command; 0 leaves only the script's own timeout.
	Timeout time.Duration
	// MaxOutput is sent as max_output (default DefaultMaxOutput).
	MaxOutput int64

	mu     sync.Mutex
	nextID int64
	label  string
}

var _ Source = (*Client)(nil)

// Label reports the host's name and version from its last hello, and its address.
func (c *Client) Label() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.label == "" {
		return "host at " + c.Addr
	}
	return c.label + " (" + c.Addr + ")"
}

// Commands asks the host for its commands (hello) and wraps each as a shell command.
func (c *Client) Commands(ctx context.Context) ([]engine.Command, error) {
	hello, err := c.Hello(ctx)
	if err != nil {
		return nil, err
	}
	label := strings.TrimSpace(hello.Name + " " + hello.Version)
	c.mu.Lock()
	c.label = label
	c.mu.Unlock()
	return c.CommandsFrom(hello), nil
}

func (c *Client) Hello(ctx context.Context) (HelloResult, error) {
	resp, err := c.roundTrip(ctx, Request{Method: MethodHello, Protocol: ProtocolVersion})
	if err != nil {
		return HelloResult{}, err
	}
	return resp.HelloResult, nil
}

func (c *Client) Invoke(ctx context.Context, cmd string, args []string, stdin, cwd string) (InvokeResult, error) {
	script := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	req := Request{Method: MethodInvoke, Cmd: cmd, Args: args, Stdin: stdin, Cwd: cwd, MaxOutput: c.MaxOutput}
	if req.MaxOutput <= 0 {
		req.MaxOutput = DefaultMaxOutput
	}
	// Tell the host how long it has, so it can stop on its own; at least 1 ms, since 0 means none.
	if deadline, ok := ctx.Deadline(); ok {
		req.TimeoutMs = max(time.Until(deadline).Milliseconds(), 1)
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return InvokeResult{}, c.timeoutErr(script)
		}
		return InvokeResult{}, err
	}
	return resp.InvokeResult, nil
}

// timeoutErr names the limit that ran out: the script's, or else the per-command Timeout.
func (c *Client) timeoutErr(script context.Context) error {
	if script.Err() == nil && c.Timeout > 0 {
		return fmt.Errorf("timed out after %s (host command limit)", c.Timeout)
	}
	return fmt.Errorf("timed out (script time limit reached)")
}

// Help asks the host for one command's full help.
func (c *Client) Help(ctx context.Context, cmd string) (string, error) {
	resp, err := c.roundTrip(ctx, Request{Method: MethodHelp, Cmd: cmd})
	if err != nil {
		return "", err
	}
	return resp.Stdout, nil
}

// CommandsFrom turns a hello result into shell commands that forward to this host.
func (c *Client) CommandsFrom(hello HelloResult) []engine.Command {
	cmds := make([]engine.Command, 0, len(hello.Commands))
	for _, info := range hello.Commands {
		if info.Name == "" {
			continue
		}
		name := info.Name
		cmd := engine.Command{
			Name:    name,
			Summary: info.Summary,
			Help:    info.Help,
			Source:  "host",
			Run: func(ctx context.Context, inv *engine.Invocation) int {
				return c.runCommand(ctx, name, inv)
			},
		}
		if cmd.Help == "" {
			cmd.HelpFunc = func(ctx context.Context) (string, error) { return c.Help(ctx, name) }
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}

func (c *Client) runCommand(ctx context.Context, name string, inv *engine.Invocation) int {
	stdin, err := ReadStdin(inv, c.MaxStdin)
	if err != nil {
		fmt.Fprintf(inv.Stderr, "%s: %v\n", name, err)
		return 1
	}
	res, err := c.Invoke(ctx, name, inv.Args, stdin, inv.Dir)
	if err != nil {
		fmt.Fprintf(inv.Stderr, "%s: %v\n", name, err)
		return 1
	}
	WriteLine(inv.Stdout, res.Stdout)
	WriteLine(inv.Stderr, res.Stderr)
	return int(res.ExitCode)
}

func (c *Client) roundTrip(ctx context.Context, req Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	req.ID = c.nextID
	req.Token = c.Token

	dialTimeout := c.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		msg := fmt.Sprintf("could not connect to host at %s: %v", c.Addr, err)
		if c.Hint != "" {
			msg += "\n(" + c.Hint + ")"
		}
		return Response{}, fmt.Errorf("%s", msg)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	payload, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return Response{}, c.ioErr(ctx, "write to host", err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxResponse)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Response{}, c.ioErr(ctx, "read from host", err)
	}
	if err != nil && len(line) >= maxResponse {
		return Response{}, fmt.Errorf("host response exceeds %d MB", maxResponse>>20)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("invalid response from host: %w", err)
	}
	if resp.Error != "" {
		return Response{}, fmt.Errorf("host error: %s", resp.Error)
	}
	if resp.ID != 0 && int64(resp.ID) != req.ID {
		return Response{}, fmt.Errorf("host answered request %d, expected %d", resp.ID, req.ID)
	}
	return resp, nil
}

func (c *Client) ioErr(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", op, ctx.Err())
	}
	return fmt.Errorf("%s failed: %w", op, err)
}
