// Package mcphost makes an existing MCP server a source of shell commands. The server
// appears as one namespaced command whose subcommands are its tools:
//
//	blender-mcp get_object_info --object_name Cube | jq .location
//
// agent-shell is an MCP *client* here: it starts (stdio) or connects to (http/sse) the
// upstream server itself, so it coexists with the same server registered directly in
// the agent's client.
package mcphost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/itchyny/gojq"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/engine"
	"github.com/brohd11/agent-shell/host"
)

// Config describes one upstream server. Its fields mirror an `.mcp.json` mcpServers
// entry, plus agent-shell options.
type Config struct {
	Name string // namespace command name, e.g. "blender-mcp"

	Type    string // "stdio" (default when Command is set), "http" or "sse"
	Command string
	Args    []string
	Env     map[string]string // added to the inherited environment
	URL     string
	Headers map[string]string

	// HideTools keeps tools out of the shell entirely.
	HideTools []string
	// Defaults fill tool arguments the agent did not pass, when the tool's schema has
	// that property (e.g. blender-mcp's required "user_prompt"). A value of the form
	// {"$tool": NAME, "$args": {...}, "$jq": FILTER} is resolved by calling that tool
	// on the same server and filtering its JSON output, once per run (e.g. Roblox
	// Studio's studio_id from list_roblox_studios).
	Defaults map[string]any
	// ErrorPattern marks results that are errors although the server did not flag
	// them (blender-mcp returns "Error ..." as normal text): they go to stderr, exit 1.
	ErrorPattern *regexp.Regexp

	// ImageDir is where images in tool results are saved; the output shows each saved
	// image's path. Empty keeps a short placeholder instead.
	ImageDir string

	// ConnectTimeout bounds starting and initializing the server (default 60s).
	ConnectTimeout time.Duration
	// Version is reported to the upstream server as the client version.
	Version string
}

// Server is a connection to one upstream MCP server; it implements host.Source.
type Server struct {
	cfg Config
	// transport overrides cfg-based transports (tests use in-memory ones).
	transport func() (mcp.Transport, error)

	mu       sync.Mutex
	session  *mcp.ClientSession
	done     chan struct{} // closed when session ends
	stderr   atomic.Pointer[tailBuffer]
	instruct string
	schemas  map[string]any // input schema per tool, from the latest listing
	resolved map[string]any // dynamic defaults resolved since the latest listing
}

var (
	_ host.Source = (*Server)(nil)
	_ host.Closer = (*Server)(nil)
)

func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// NewWithTransport builds a Server over a custom transport factory, called on every
// (re)connect.
func NewWithTransport(cfg Config, transport func() (mcp.Transport, error)) *Server {
	return &Server{cfg: cfg, transport: transport}
}

func (s *Server) Name() string { return s.cfg.Name }

func (s *Server) Label() string {
	switch {
	case s.cfg.URL != "":
		return fmt.Sprintf("%s (%s)", s.cfg.Name, s.cfg.URL)
	case s.cfg.Command != "":
		return fmt.Sprintf("%s (%s)", s.cfg.Name, strings.TrimSpace(s.cfg.Command+" "+strings.Join(s.cfg.Args, " ")))
	}
	return s.cfg.Name
}

func (s *Server) newTransport() (mcp.Transport, error) {
	if s.transport != nil {
		return s.transport()
	}
	typ := s.cfg.Type
	if typ == "" {
		if s.cfg.URL != "" {
			typ = "http"
		} else {
			typ = "stdio"
		}
	}
	switch typ {
	case "stdio":
		if s.cfg.Command == "" {
			return nil, errors.New("no command configured")
		}
		cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
		cmd.Env = os.Environ()
		for k, v := range s.cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		tail := &tailBuffer{max: 4096}
		s.stderr.Store(tail)
		cmd.Stderr = tail
		return &mcp.CommandTransport{Command: cmd}, nil
	case "http", "streamable-http":
		return &mcp.StreamableClientTransport{Endpoint: s.cfg.URL, HTTPClient: s.httpClient()}, nil
	case "sse":
		return &mcp.SSEClientTransport{Endpoint: s.cfg.URL, HTTPClient: s.httpClient()}, nil
	}
	return nil, fmt.Errorf("unknown server type %q (want stdio, http or sse)", typ)
}

func (s *Server) httpClient() *http.Client {
	if len(s.cfg.Headers) == 0 {
		return nil
	}
	return &http.Client{Transport: headerTransport{headers: s.cfg.Headers, base: http.DefaultTransport}}
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

// connected returns a live session, starting one if needed.
func (s *Server) connected(ctx context.Context) (*mcp.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		select {
		case <-s.done:
			s.session = nil
		default:
			return s.session, nil
		}
	}
	t, err := s.newTransport()
	if err != nil {
		return nil, err
	}
	timeout := s.cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	// The session outlives this run, so it must not inherit the run's cancellation;
	// a stdio server stays up between runs.
	connectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel) // but give up connecting if the run ends
	defer stop()

	version := s.cfg.Version
	if version == "" {
		version = "dev"
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agent-shell", Version: version}, nil)
	session, err := client.Connect(connectCtx, t, nil)
	if err != nil {
		return nil, s.withStderr(fmt.Errorf("connect: %w", err))
	}
	done := make(chan struct{})
	go func() {
		session.Wait()
		close(done)
	}()
	s.session, s.done = session, done
	if init := session.InitializeResult(); init != nil {
		s.instruct = init.Instructions
	}
	return session, nil
}

// reset drops a session that failed, so the next call reconnects.
func (s *Server) reset(session *mcp.ClientSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == session {
		s.session.Close()
		s.session = nil
	}
}

func (s *Server) withStderr(err error) error {
	if buf := s.stderr.Load(); buf != nil {
		if tail := strings.TrimSpace(buf.String()); tail != "" {
			return fmt.Errorf("%w\nserver stderr:\n%s", err, tail)
		}
	}
	return err
}

// Tools lists the upstream's tools, minus hidden ones. Listing is read-only, so a
// dropped connection is retried once with a fresh session.
func (s *Server) Tools(ctx context.Context) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var session *mcp.ClientSession
		session, err = s.connected(ctx)
		if err != nil {
			return nil, err
		}
		tools, err = listTools(ctx, session)
		if err == nil {
			break
		}
		s.reset(session)
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return nil, s.withStderr(err)
	}
	schemas := make(map[string]any, len(tools))
	for _, t := range tools {
		schemas[t.Name] = t.InputSchema
	}
	s.mu.Lock()
	s.schemas = schemas
	s.resolved = nil // re-resolve dynamic defaults once per run
	s.mu.Unlock()
	hidden := map[string]bool{}
	for _, name := range s.cfg.HideTools {
		hidden[name] = true
	}
	out := tools[:0]
	for _, t := range tools {
		if !hidden[t.Name] {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func listTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// CallTool calls one tool, filling configured defaults the tool declares but args
// lack. Unlike listing, a call is never retried: it may have had side effects before
// the connection dropped.
func (s *Server) CallTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	return s.call(ctx, name, args, 0)
}

func (s *Server) call(ctx context.Context, name string, args map[string]any, depth int) (*mcp.CallToolResult, error) {
	session, err := s.connected(ctx)
	if err != nil {
		return nil, err
	}
	if args, err = s.fillDefaults(ctx, name, args, depth); err != nil {
		return nil, err
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		if errors.Is(err, mcp.ErrConnectionClosed) {
			s.reset(session)
		}
		return nil, s.withStderr(err)
	}
	return res, nil
}

func (s *Server) fillDefaults(ctx context.Context, name string, args map[string]any, depth int) (map[string]any, error) {
	if len(s.cfg.Defaults) == 0 {
		return args, nil
	}
	s.mu.Lock()
	props := properties(s.schemas[name])
	s.mu.Unlock()
	filled := make(map[string]any, len(args)+len(s.cfg.Defaults))
	for k, v := range args {
		filled[k] = v
	}
	keys := make([]string, 0, len(s.cfg.Defaults))
	for k := range s.cfg.Defaults {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, declared := props[k]; !declared {
			continue
		}
		if _, set := filled[k]; set {
			continue
		}
		v, err := s.defaultValue(ctx, k, s.cfg.Defaults[k], depth)
		if err != nil {
			return nil, fmt.Errorf("--%s was not given and its default failed: %w", k, err)
		}
		filled[k] = v
	}
	return filled, nil
}

// DynamicDefault is a default computed by calling a tool.
type DynamicDefault struct {
	Tool string
	Args map[string]any
	JQ   *gojq.Code // nil: the trimmed text result
}

// ParseDynamicDefault recognizes {"$tool": ..., "$args": ..., "$jq": ...}. It returns
// ok=false for ordinary values, and an error for a malformed spec.
func ParseDynamicDefault(v any) (d *DynamicDefault, ok bool, err error) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return nil, false, nil
	}
	rawTool, has := m["$tool"]
	if !has {
		return nil, false, nil
	}
	tool, _ := rawTool.(string)
	if tool == "" {
		return nil, true, errors.New(`"$tool" must be a tool name`)
	}
	d = &DynamicDefault{Tool: tool}
	for k, val := range m {
		switch k {
		case "$tool":
		case "$args":
			args, ok := val.(map[string]any)
			if !ok {
				return nil, true, errors.New(`"$args" must be an object`)
			}
			d.Args = args
		case "$jq":
			src, _ := val.(string)
			if d.JQ, err = CompileJQ(src); err != nil {
				return nil, true, fmt.Errorf(`"$jq": %w`, err)
			}
		default:
			return nil, true, fmt.Errorf("unknown key %q (want $tool, $args, $jq)", k)
		}
	}
	return d, true, nil
}

// CompileJQ compiles a jq filter from config. The filter cannot read the environment.
func CompileJQ(src string) (*gojq.Code, error) {
	q, err := gojq.Parse(src)
	if err != nil {
		return nil, err
	}
	return gojq.Compile(q, gojq.WithEnvironLoader(func() []string { return nil }))
}

// ErrNotJSON is returned by RunJQ when its input is not JSON.
var ErrNotJSON = errors.New("not JSON")

// RunJQ runs a compiled filter over text holding one JSON value, as `jq -r` would print
// it: string results raw, others as compact JSON, one per line.
func RunJQ(ctx context.Context, code *gojq.Code, text string) (string, error) {
	input, err := decodeJSON(text)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotJSON, err)
	}
	var lines []string
	iter := code.RunWithContext(ctx, input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		switch v := v.(type) {
		case error:
			return "", jqError(v)
		case string:
			lines = append(lines, v)
		default:
			b, err := gojq.Marshal(v)
			if err != nil {
				return "", err
			}
			lines = append(lines, string(b))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// jqError shows error("message") from a filter verbatim, without gojq's prefix.
func jqError(err error) error {
	var ve gojq.ValueError
	if errors.As(err, &ve) {
		if msg, ok := ve.Value().(string); ok {
			return errors.New(msg)
		}
	}
	return err
}

func (s *Server) defaultValue(ctx context.Context, key string, v any, depth int) (any, error) {
	d, dynamic, err := ParseDynamicDefault(v)
	if err != nil {
		return nil, err
	}
	if !dynamic {
		return v, nil
	}
	s.mu.Lock()
	cached, ok := s.resolved[key]
	s.mu.Unlock()
	if ok {
		return cached, nil
	}
	if depth >= 2 {
		return nil, errors.New("dynamic defaults nest too deeply")
	}
	res, err := s.call(ctx, d.Tool, d.Args, depth+1)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Tool, err)
	}
	text := ResultText(res)
	if res.IsError {
		return nil, fmt.Errorf("%s: %s", d.Tool, text)
	}
	var value any = strings.TrimSpace(text)
	if d.JQ != nil {
		input, err := decodeJSON(text)
		if err != nil {
			return nil, fmt.Errorf("%s did not return JSON: %w", d.Tool, err)
		}
		out, ok := d.JQ.RunWithContext(ctx, input).Next()
		if !ok || out == nil {
			return nil, fmt.Errorf("%s: no value found", d.Tool)
		}
		if err, isErr := out.(error); isErr {
			return nil, jqError(err)
		}
		value = out
	}
	s.mu.Lock()
	if s.resolved == nil {
		s.resolved = map[string]any{}
	}
	s.resolved[key] = value
	s.mu.Unlock()
	return value, nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil
	}
	err := s.session.Close()
	s.session = nil
	return err
}

// Commands returns the namespace command. When the server is unreachable it still
// returns the command, as a stub reporting why, alongside the error.
func (s *Server) Commands(ctx context.Context) ([]engine.Command, error) {
	tools, err := s.Tools(ctx)
	if err != nil {
		msg := err.Error()
		return []engine.Command{{
			Name:    s.cfg.Name,
			Summary: "MCP server (unavailable)",
			Source:  "mcp",
			Run: func(ctx context.Context, inv *engine.Invocation) int {
				fmt.Fprintf(inv.Stderr, "%s: MCP server unavailable: %s\n", s.cfg.Name, msg)
				return 1
			},
		}}, err
	}
	s.mu.Lock()
	instructions := s.instruct
	s.mu.Unlock()
	ns := &namespace{server: s, tools: tools, instructions: instructions}
	return []engine.Command{{
		Name:    s.cfg.Name,
		Summary: ns.summary(),
		Help:    ns.help(),
		Source:  "mcp",
		Run:     ns.run,
	}}, nil
}

// tailBuffer keeps the last max bytes written; the upstream's stderr goes here so
// connection errors can show why a server died.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
