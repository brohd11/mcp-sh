package host_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brohd11/mcp-sh/host"
	"github.com/brohd11/mcp-sh/hosttest"
)

func startHost(t *testing.T, handlers map[string]hosttest.Handler) (*hosttest.Host, string) {
	t.Helper()
	h := &hosttest.Host{Name: "test", Handlers: handlers}
	addr, err := h.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, addr
}

func say(req host.Request) host.InvokeResult {
	return host.InvokeResult{Stdout: strings.Join(req.Args, " ")}
}

// The host learns how long it has and how much output to keep, so it can stop on its own.
func TestInvokeSendsLimits(t *testing.T) {
	h, addr := startHost(t, map[string]hosttest.Handler{"say": say})
	c := &host.Client{Addr: addr}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Invoke(ctx, "say", []string{"hi"}, "", "/"); err != nil {
		t.Fatal(err)
	}
	req := h.Invokes()[0]
	if req.TimeoutMs <= 0 || req.TimeoutMs > 5000 {
		t.Errorf("timeout_ms = %d, want 1..5000", req.TimeoutMs)
	}
	if req.MaxOutput != host.DefaultMaxOutput {
		t.Errorf("max_output = %d, want %d", req.MaxOutput, host.DefaultMaxOutput)
	}
}

// Without a deadline nothing is sent, so the host imposes no limit of its own.
func TestInvokeWithoutDeadline(t *testing.T) {
	h, addr := startHost(t, map[string]hosttest.Handler{"say": say})
	c := &host.Client{Addr: addr, MaxOutput: 100}
	if _, err := c.Invoke(context.Background(), "say", nil, "", "/"); err != nil {
		t.Fatal(err)
	}
	req := h.Invokes()[0]
	if req.TimeoutMs != 0 || req.MaxOutput != 100 {
		t.Errorf("got timeout_ms %d, max_output %d; want 0, 100", req.TimeoutMs, req.MaxOutput)
	}
}

// A per-command limit gives up on a slow host and says which limit ran out.
func TestInvokeHostTimeout(t *testing.T) {
	h, addr := startHost(t, map[string]hosttest.Handler{"slow": func(req host.Request) host.InvokeResult {
		time.Sleep(500 * time.Millisecond)
		return host.InvokeResult{}
	}})
	c := &host.Client{Addr: addr, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := c.Invoke(context.Background(), "slow", nil, "", "/")
	if err == nil || !strings.Contains(err.Error(), "host command limit") {
		t.Fatalf("err = %v, want a host command limit timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("gave up after %s, want ~100ms", elapsed)
	}
	if got := h.Invokes()[0].TimeoutMs; got <= 0 || got > 100 {
		t.Errorf("timeout_ms = %d, want 1..100", got)
	}
}
