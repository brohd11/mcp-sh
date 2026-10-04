// Package hosttest is an in-process host speaking the host protocol, for tests of
// mcp-sh itself and of host binaries' Go-side builtins. It is also the smallest
// complete reference for what a host must implement.
package hosttest

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"sync"

	"github.com/brohd11/mcp-sh/host"
)

// Handler answers one invoke.
type Handler func(req host.Request) host.InvokeResult

type Host struct {
	Name     string
	Version  string
	Token    string // when set, requests must carry it
	Commands []host.CommandInfo
	Handlers map[string]Handler
	// HelpTexts answers the optional help method; nil means the host does not
	// implement it.
	HelpTexts map[string]string
	// GodotNumbers writes integers as floats ("exit_code":0.0), as Godot's JSON does.
	GodotNumbers bool

	ln    net.Listener
	mu    sync.Mutex
	calls []host.Request
	wg    sync.WaitGroup
}

// Start listens on a free loopback port and serves until Close.
func (h *Host) Start() (addr string, err error) {
	h.ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		for {
			conn, err := h.ln.Accept()
			if err != nil {
				return
			}
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				h.serve(conn)
			}()
		}
	}()
	return h.ln.Addr().String(), nil
}

func (h *Host) Close() {
	if h.ln != nil {
		h.ln.Close()
	}
	h.wg.Wait()
}

// Calls returns every request received so far.
func (h *Host) Calls() []host.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]host.Request(nil), h.calls...)
}

// Invokes returns the received invoke requests.
func (h *Host) Invokes() []host.Request {
	var out []host.Request
	for _, c := range h.Calls() {
		if c.Method == host.MethodInvoke {
			out = append(out, c)
		}
	}
	return out
}

var intField = regexp.MustCompile(`"(id|exit_code)":(-?\d+)([,}])`)

func (h *Host) serve(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	resp := h.handle(line)
	b, _ := json.Marshal(resp)
	if h.GodotNumbers {
		b = intField.ReplaceAll(b, []byte(`"$1":$2.0$3`))
	}
	conn.Write(append(b, '\n'))
}

func (h *Host) handle(line []byte) map[string]any {
	var req host.Request
	if err := json.Unmarshal(line, &req); err != nil {
		return map[string]any{"error": "bad request: " + err.Error()}
	}
	h.mu.Lock()
	h.calls = append(h.calls, req)
	h.mu.Unlock()
	fail := func(msg string) map[string]any { return map[string]any{"id": req.ID, "error": msg} }
	if h.Token != "" && req.Token != h.Token {
		return fail("Unauthorized")
	}
	switch req.Method {
	case host.MethodHello:
		return map[string]any{"id": req.ID, "name": h.Name, "version": h.Version, "commands": h.Commands}
	case host.MethodInvoke:
		handler, ok := h.Handlers[req.Cmd]
		if !ok {
			return fail(errors.New("unknown command " + req.Cmd).Error())
		}
		res := handler(req)
		return map[string]any{"id": req.ID, "stdout": res.Stdout, "stderr": res.Stderr, "exit_code": int(res.ExitCode)}
	case host.MethodHelp:
		if text, ok := h.HelpTexts[req.Cmd]; ok {
			return map[string]any{"id": req.ID, "stdout": text}
		}
	}
	return fail("unknown method " + req.Method)
}
