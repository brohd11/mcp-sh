package shell_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brohd11/agent-shell/engine/shengine"
	"github.com/brohd11/agent-shell/host"
	"github.com/brohd11/agent-shell/hosttest"
	"github.com/brohd11/agent-shell/shell"
)

func fakeGodot() *hosttest.Host {
	return &hosttest.Host{
		Name: "godot", Version: "4.5",
		GodotNumbers: true,
		Commands: []host.CommandInfo{
			{Name: "nodes", Summary: "list scene nodes as JSON lines"},
			{Name: "say", Summary: "echo args", Help: "usage: say [words...]"},
			{Name: "count", Summary: "count stdin lines"},
			{Name: "fail", Summary: "always fails"},
			{Name: "noisy", Summary: "lots of output"},
			{Name: "lazy-help", Summary: "help served by the help method"},
			{Name: "test", Summary: "run project tests"},
		},
		Handlers: map[string]hosttest.Handler{
			"nodes": func(req host.Request) host.InvokeResult {
				var lines []string
				for _, n := range []struct{ Name, Type string }{{"Player", "CharacterBody2D"}, {"Sprite", "Sprite2D"}, {"Cam", "Camera2D"}} {
					b, _ := json.Marshal(map[string]string{"name": n.Name, "type": n.Type})
					lines = append(lines, string(b))
				}
				return host.InvokeResult{Stdout: strings.Join(lines, "\n")} // no trailing newline, like Godot
			},
			"say": func(req host.Request) host.InvokeResult {
				b, _ := json.Marshal(req.Args)
				return host.InvokeResult{Stdout: string(b)}
			},
			"count": func(req host.Request) host.InvokeResult {
				return host.InvokeResult{Stdout: fmt.Sprint(strings.Count(req.Stdin, "\n"))}
			},
			"fail": func(req host.Request) host.InvokeResult {
				return host.InvokeResult{Stderr: "it broke", ExitCode: 3}
			},
			"noisy": func(req host.Request) host.InvokeResult {
				return host.InvokeResult{Stdout: strings.Repeat("x", 5000)}
			},
			"test": func(req host.Request) host.InvokeResult {
				return host.InvokeResult{Stdout: "ran tests " + strings.Join(req.Args, " ")}
			},
			"lazy-help": func(req host.Request) host.InvokeResult {
				return host.InvokeResult{Stdout: "lazy-help ran"}
			},
		},
		HelpTexts: map[string]string{"lazy-help": "usage: lazy-help (from help method)"},
	}
}

func newShell(t *testing.T, h *hosttest.Host, token string) *shell.Shell {
	t.Helper()
	addr, err := h.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return &shell.Shell{
		Sources: []host.Source{&host.Client{Addr: addr, Token: token}},
		Engine:  shengine.New(shengine.Options{}),
	}
}

func TestPipelineThroughHost(t *testing.T) {
	sh := newShell(t, fakeGodot(), "")
	res := sh.Run(context.Background(), `nodes | jq -r 'select(.type | endswith("2D")) | .name' | sort`, 0)
	if res.Err != nil || res.ExitCode != 0 || res.Stdout != "Cam\nPlayer\nSprite\n" {
		t.Fatalf("got %+v", res)
	}
}

func TestArgsStdinAndCwdForwarded(t *testing.T) {
	h := fakeGodot()
	sh := newShell(t, h, "")
	res := sh.Run(context.Background(), `name="a b"; printf 'x\ny\n' | say "$name" 'c"d' --flag=1`, 0)
	if res.Stdout != `["a b","c\"d","--flag=1"]`+"\n" {
		t.Fatalf("got %+v", res)
	}
	inv := h.Invokes()
	if len(inv) != 1 || inv[0].Stdin != "x\ny\n" || inv[0].Cwd != "/" || inv[0].Cmd != "say" {
		t.Fatalf("host saw %+v", inv)
	}
}

func TestHostToHostPipeline(t *testing.T) {
	// Both stages are host commands; invokes are serialized without deadlocking.
	sh := newShell(t, fakeGodot(), "")
	res := sh.Run(context.Background(), `nodes | count && nodes | grep Cam | count`, 0)
	if res.Stdout != "3\n1\n" || res.ExitCode != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestHostExitCodeAndStderr(t *testing.T) {
	sh := newShell(t, fakeGodot(), "")
	res := sh.Run(context.Background(), `fail || echo "status=$?"; fail`, 0)
	if res.Stdout != "status=3\n" || res.Stderr != "it broke\nit broke\n" || res.ExitCode != 3 {
		t.Fatalf("got %+v", res)
	}
	text := res.Text()
	if !strings.Contains(text, "[stderr]\nit broke") || !strings.HasSuffix(text, "[exit_code=3]") {
		t.Fatalf("text:\n%s", text)
	}
}

func TestToken(t *testing.T) {
	h := fakeGodot()
	h.Token = "s3cret"
	if res := newShell(t, h, "wrong").Run(context.Background(), `nodes`, 0); res.Err == nil || !strings.Contains(res.Err.Error(), "Unauthorized") {
		t.Fatalf("wrong token: got %+v", res)
	}
	h2 := fakeGodot()
	h2.Token = "s3cret"
	if res := newShell(t, h2, "s3cret").Run(context.Background(), `nodes | wc -l`, 0); res.Stdout != "3\n" {
		t.Fatalf("right token: got %+v", res)
	}
}

func TestHostUnreachable(t *testing.T) {
	sh := &shell.Shell{
		Sources: []host.Source{&host.Client{Addr: "127.0.0.1:1", Hint: "run 'mcp bridge start' in the editor"}},
		Engine:  shengine.New(shengine.Options{}),
	}
	res := sh.Run(context.Background(), `echo hi`, 0)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "mcp bridge start") || res.ExitCode == 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestTruncation(t *testing.T) {
	sh := newShell(t, fakeGodot(), "")
	sh.MaxOutput = 1000
	res := sh.Run(context.Background(), `noisy; echo after`, 0)
	if len(res.Stdout) != 1000 || !res.Truncated || res.ExitCode != 0 {
		t.Fatalf("len %d truncated %v exit %d", len(res.Stdout), res.Truncated, res.ExitCode)
	}
	if !strings.Contains(res.Text(), "[output truncated") {
		t.Fatal("no truncation notice")
	}
}

func TestTimeout(t *testing.T) {
	sh := newShell(t, fakeGodot(), "")
	start := time.Now()
	res := sh.Run(context.Background(), `while true; do nodes >/dev/null; done`, 300*time.Millisecond)
	if res.ExitCode != 124 || res.Err == nil || !strings.Contains(res.Err.Error(), "timed out after 300ms") {
		t.Fatalf("got %+v", res)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestHelpForHostCommands(t *testing.T) {
	h := fakeGodot()
	sh := newShell(t, h, "")
	res := sh.Run(context.Background(), `help say; help lazy-help; help nodes; help | head -1`, 0)
	want := "usage: say [words...]\nusage: lazy-help (from help method)\nnodes: list scene nodes as JSON lines\nHost commands:\n"
	if res.Stdout != want {
		t.Fatalf("got %q", res.Stdout)
	}
	// Help must never run the command itself.
	if inv := h.Invokes(); len(inv) != 0 {
		t.Fatalf("help invoked commands: %+v", inv)
	}
}

func TestReservedNameReachableViaHost(t *testing.T) {
	sh := newShell(t, fakeGodot(), "")
	// Bare `test` is bash's test builtin; `host test` reaches the host command.
	res := sh.Run(context.Background(), `test 1 -eq 1 && echo builtin; host test res://tests; help | grep '^  test '`, 0)
	want := "builtin\nran tests res://tests\n  test       run project tests (the shell's own test hides it; run as: host test ...)\n"
	if res.Stdout != want {
		t.Fatalf("got %q", res.Stdout)
	}
	if res := sh.Run(context.Background(), `host nope`, 0); res.ExitCode != 127 {
		t.Fatalf("host nope: %+v", res)
	}
}

func TestOneSourceDownIsAWarning(t *testing.T) {
	h := fakeGodot()
	addr, err := h.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sh := &shell.Shell{
		Sources: []host.Source{
			&host.Client{Addr: addr},
			&host.Client{Addr: "127.0.0.1:1"},
		},
		Engine: shengine.New(shengine.Options{}),
	}
	res := sh.Run(context.Background(), `nodes | wc -l`, 0)
	if res.Err != nil || res.Stdout != "3\n" || len(res.Warnings) != 1 || !strings.Contains(res.Text(), "[warning] host at 127.0.0.1:1 unavailable") {
		t.Fatalf("got %+v\n%s", res, res.Text())
	}
	_, statuses, _ := sh.Commands(context.Background())
	if got := shell.FormatSources(statuses); !strings.HasPrefix(got, "Sources:\n  godot 4.5 (") || !strings.Contains(got, ": 7 commands\n") {
		t.Fatalf("sources: %q", got)
	}
}

func TestResultText(t *testing.T) {
	if got := (shell.Result{}).Text(); got != "(no output, exit_code=0)" {
		t.Fatalf("empty: %q", got)
	}
	if got := (shell.Result{Stdout: "a\n"}).Text(); got != "a" {
		t.Fatalf("plain: %q", got)
	}
}
