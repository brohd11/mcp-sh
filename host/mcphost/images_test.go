package mcphost_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brohd11/agent-shell/host/mcphost"
)

var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\nfake png")
	jpegBytes = []byte("\xff\xd8\xff fake jpeg")
)

// imageServer has one tool returning text, an image, and an embedded image resource.
func imageServer(t *testing.T, imageDir string) *mcphost.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "img", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "snap", Description: "Take a picture."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "caption"},
				&mcp.ImageContent{MIMEType: "image/png", Data: pngBytes},
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "file:///shot.jpg", MIMEType: "image/jpeg", Blob: jpegBytes}},
			}}, nil, nil
		})
	return mcphost.NewWithTransport(mcphost.Config{Name: "img", ImageDir: imageDir}, func() (mcp.Transport, error) {
		st, ct := mcp.NewInMemoryTransports()
		if _, err := s.Connect(context.Background(), st, nil); err != nil {
			return nil, err
		}
		return ct, nil
	})
}

func TestImagesSavedToDisk(t *testing.T) {
	dir := t.TempDir()
	sh := newShell(t, imageServer(t, dir))
	res := run(t, sh, `img snap`)
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if res.ExitCode != 0 || len(lines) != 3 || lines[0] != "caption" {
		t.Fatalf("got %q (exit %d, stderr %q)", res.Stdout, res.ExitCode, res.Stderr)
	}
	for i, want := range []struct {
		ext  string
		data []byte
	}{{".png", pngBytes}, {".jpg", jpegBytes}} {
		path := lines[i+1]
		if !filepath.IsAbs(path) || filepath.Dir(path) != dir || !strings.HasPrefix(filepath.Base(path), "img-snap-") || filepath.Ext(path) != want.ext {
			t.Fatalf("path %q: want an absolute %s file named img-snap-* in %s", path, want.ext, dir)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(want.data) {
			t.Fatalf("%s: got %q, %v", path, got, err)
		}
	}

	// The path is a line of its own, so it pipes.
	res = run(t, sh, `img snap | grep -c '\.png$'`)
	if res.Stdout != "1\n" {
		t.Fatalf("pipe: %q", res.Stdout)
	}
}

func TestImagesWithoutDirKeepPlaceholder(t *testing.T) {
	sh := newShell(t, imageServer(t, ""))
	res := run(t, sh, `img snap`)
	want := fmt.Sprintf("caption\n[image image/png, %d bytes]\n[resource file:///shot.jpg, %d bytes]\n", len(pngBytes), len(jpegBytes))
	if res.Stdout != want {
		t.Fatalf("got %q want %q", res.Stdout, want)
	}
}

func TestImageDirIsPruned(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for i := range 60 {
		p := filepath.Join(dir, fmt.Sprintf("old-%02d.png", i))
		os.WriteFile(p, nil, 0o644)
		os.Chtimes(p, old, old.Add(time.Duration(i)*time.Second))
	}
	sh := newShell(t, imageServer(t, dir))
	if res := run(t, sh, `img snap`); res.ExitCode != 0 {
		t.Fatalf("snap: %+v", res)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 50 {
		t.Fatalf("kept %d files, want 50", len(entries))
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	// The two new images stay, plus the 48 newest old ones (old-12 .. old-59).
	if names["old-11.png"] || !names["old-12.png"] || !names["old-59.png"] {
		t.Fatalf("pruned the wrong files: %v", names)
	}
}
