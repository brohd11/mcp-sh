// Package host is the server side of the host protocol: the link between agent-shell
// and the program that implements commands (Godot, Blender, ...).
//
// # Protocol v1
//
// The host listens on a loopback TCP port. agent-shell connects, writes one JSON
// request line, reads one JSON response line, and closes. Every request carries the
// optional shared-secret token.
//
//	→ {"id":1,"method":"hello","token":"..."}
//	← {"id":1,"name":"godot","version":"4.5","commands":[{"name":"scene","summary":"...","help":"..."}]}
//
//	→ {"id":2,"method":"invoke","token":"...","cmd":"scene","args":["tree"],"stdin":"","cwd":"/"}
//	← {"id":2,"stdout":"...","stderr":"...","exit_code":0}
//
//	→ {"id":3,"method":"help","token":"...","cmd":"scene"}            (optional)
//	← {"id":3,"stdout":"usage: scene ..."}
//
// Any response may instead be {"id":N,"error":"message"} (bad token, unknown method,
// unknown command). Numbers may arrive as floats (Godot's JSON parser makes every
// number a float), so integer fields are decoded leniently.
//
// `help` is only asked for commands whose hello entry has no help text, so a host can
// keep hello small and serve full help lazily. A host without it just answers with an
// error, and the summary is shown instead. agent-shell never invokes `cmd --help` to
// get help: a command that ignored the flag would run for real.
package host

import (
	"encoding/json"
	"fmt"
	"math"
)

const ProtocolVersion = 1

const (
	MethodHello  = "hello"
	MethodInvoke = "invoke"
	MethodHelp   = "help"
)

type Request struct {
	ID       int64    `json:"id"`
	Method   string   `json:"method"`
	Token    string   `json:"token,omitempty"`
	Protocol int      `json:"protocol,omitempty"`
	Cmd      string   `json:"cmd,omitempty"`
	Args     []string `json:"args,omitempty"`
	Stdin    string   `json:"stdin,omitempty"`
	Cwd      string   `json:"cwd,omitempty"`
}

// CommandInfo describes one host command, as sent in the hello response.
type CommandInfo struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	Help    string `json:"help,omitempty"`
}

type HelloResult struct {
	Name     string        `json:"name"`
	Version  string        `json:"version,omitempty"`
	Commands []CommandInfo `json:"commands"`
}

type InvokeResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode Int    `json:"exit_code"`
}

// Response is the union of every result shape, plus the error field.
type Response struct {
	ID    Int    `json:"id"`
	Error string `json:"error,omitempty"`
	HelloResult
	InvokeResult
}

// Int decodes from a JSON integer or an integral float (1 or 1.0).
type Int int

func (n *Int) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*n = 0
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	if f != math.Trunc(f) || f > math.MaxInt32 || f < math.MinInt32 {
		return fmt.Errorf("expected an integer, got %v", f)
	}
	*n = Int(f)
	return nil
}
