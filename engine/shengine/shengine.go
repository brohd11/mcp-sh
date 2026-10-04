// Package shengine is the default engine: a POSIX/bash interpreter (mvdan.cc/sh) with
// every side effect routed through handlers we control.
//
// Sandbox model:
//   - Commands: only names in the run's Registry execute. No OS process is ever started;
//     anything unknown fails with status 127.
//   - Files: redirects, `source`, globbing, `cd` and `test -f` all go through the
//     open/stat/readdir/access handlers. With no Root they only see /dev/null (and globs
//     match nothing, staying literal); with a
//     Root they are confined to that directory via os.Root (symlink escapes included).
//   - Process substitution is rejected before running, since it creates FIFOs on disk.
//   - Environment starts empty apart from Options.Env; nothing leaks from the server.
//   - Time is bounded by the caller's context.
package shengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/brohd11/agent-shell/engine"
)

// ErrFileAccessDisabled is returned for any file operation when Options.Root is empty.
var ErrFileAccessDisabled = errors.New("file access is disabled in this shell")

// helpAlias is what the bash `help` builtin is renamed to when the registry has its own
// `help`, so agents get our command list instead of bash's builtin help.
const helpAlias = "\x00help"

// pwdAlias is what the bash `pwd` builtin is renamed to. The builtin prints $PWD, which on
// Windows holds the interpreter's backslash form ("\sub"), and `pwd -P` would resolve
// symlinks on the real filesystem. The replacement prints the virtual directory.
const pwdAlias = "\x00pwd"

type Options struct {
	// Root is a host directory exposed to scripts as "/". Empty disables file access.
	Root string
	// Env is the initial environment ("KEY=value"). The server's own environment is
	// never inherited.
	Env []string
}

type Engine struct {
	opts Options
}

var (
	_ engine.Engine   = (*Engine)(nil)
	_ engine.Reserver = (*Engine)(nil)
)

func New(opts Options) *Engine {
	return &Engine{opts: opts}
}

// Parse parses a script as bash and rejects constructs the sandbox cannot contain.
func Parse(script string) (*syntax.File, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	if err != nil {
		return nil, err
	}
	var bad error
	syntax.Walk(file, func(node syntax.Node) bool {
		if bad != nil {
			return false
		}
		if ps, ok := node.(*syntax.ProcSubst); ok {
			bad = fmt.Errorf("%s: process substitution is not supported; use a pipe or a variable", ps.Pos())
		}
		return true
	})
	if bad != nil {
		return nil, bad
	}
	return file, nil
}

// Reserved reports whether name is a bash keyword or interpreter builtin, which a
// registry command cannot override. `help` is exempt: it is routed to the registry.
func (e *Engine) Reserved(name string) bool {
	return name != "help" && (interp.IsBuiltin(name) || syntax.IsKeyword(name))
}

func (e *Engine) Run(ctx context.Context, script string, cmds engine.Registry, stdio engine.IO) (int, error) {
	file, err := Parse(script)
	if err != nil {
		return 2, err
	}

	sb, err := newSandbox(e.opts.Root)
	if err != nil {
		return 1, err
	}
	defer sb.close()

	runner, err := interp.New(
		interp.Env(expand.ListEnviron(e.opts.Env...)),
		interp.StdIO(stdio.Stdin, stdio.Stdout, stdio.Stderr),
		interp.CallHandler(callHandler(cmds)),
		interp.ExecHandlers(execMiddleware(cmds, sb)),
		interp.OpenHandler(sb.open),
		interp.StatHandler(sb.stat),
		interp.ReadDirHandler2(sb.readDir),
		interp.AccessHandler(sb.access),
	)
	if err != nil {
		return 1, err
	}
	// Scripts see a virtual "/" whatever the server's real working directory is. Set
	// directly: the Dir option would os.Stat a real path.
	runner.Dir = "/"

	err = runner.Run(ctx, file)
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 124, fmt.Errorf("script timed out")
		}
		return 130, fmt.Errorf("script cancelled")
	}
	if err == nil {
		return 0, nil
	}
	if status, ok := interp.IsExitStatus(err); ok {
		return int(status), nil
	}
	return 1, err
}

func callHandler(cmds engine.Registry) interp.CallHandlerFunc {
	_, hasHelp := cmds.Lookup("help")
	return func(ctx context.Context, args []string) ([]string, error) {
		switch {
		case hasHelp && args[0] == "help":
			args[0] = helpAlias
		case args[0] == "pwd":
			args[0] = pwdAlias
		}
		return args, nil
	}
}

func execMiddleware(cmds engine.Registry, sb *sandbox) func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	// next (the OS exec handler) is deliberately never called.
	return func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			hc := interp.HandlerCtx(ctx)
			name := args[0]
			if name == pwdAlias {
				return pwd(hc, args[1:])
			}
			if name == helpAlias {
				name = "help"
			}
			cmd, ok := cmds.Lookup(name)
			if !ok {
				fmt.Fprintf(hc.Stderr, "%s: command not found (run 'help' to list commands)\n", name)
				return interp.ExitStatus(127)
			}
			stdin := hc.Stdin
			if stdin == nil {
				stdin = strings.NewReader("")
			}
			dir := toVirtual(hc.Dir)
			inv := &engine.Invocation{
				Args:    args[1:],
				Stdin:   stdin,
				Stdout:  hc.Stdout,
				Stderr:  hc.Stderr,
				Dir:     dir,
				Resolve: func(p string) (string, error) { return resolve(dir, p) },
			}
			if sb.root != nil {
				inv.FS = sb.root.FS()
			}
			code := cmd.Run(ctx, inv)
			if code == 0 {
				return nil
			}
			if code < 0 || code > 255 {
				code = 1
			}
			return interp.ExitStatus(uint8(code))
		}
	}
}

// pwd prints the virtual working directory. -L and -P are accepted; with no real
// filesystem behind "/" there are no symlinks to resolve.
func pwd(hc interp.HandlerContext, args []string) error {
	for _, a := range args {
		if a != "-L" && a != "-P" {
			fmt.Fprintf(hc.Stderr, "pwd: invalid option: %q\n", a)
			return interp.ExitStatus(2)
		}
	}
	fmt.Fprintln(hc.Stdout, toVirtual(hc.Dir))
	return nil
}

// toVirtual normalises an interpreter directory to slash form ("/a/b").
func toVirtual(dir string) string {
	dir = filepath.ToSlash(dir)
	if vol := filepath.VolumeName(dir); vol != "" { // Windows: "C:/x" -> "/x"
		dir = strings.TrimPrefix(dir, vol)
	}
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	return path.Clean(dir)
}

// resolve maps a shell path to an fs.ValidPath relative to the sandbox root. Cleaning
// an absolute path can never climb above "/", so ".." cannot escape lexically; os.Root
// handles symlinks.
func resolve(dir, p string) (string, error) {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = path.Join(toVirtual(dir), p)
	}
	rel := strings.TrimPrefix(path.Clean(p), "/")
	if rel == "" {
		rel = "."
	}
	if !fs.ValidPath(rel) {
		return "", &fs.PathError{Op: "resolve", Path: p, Err: fs.ErrInvalid}
	}
	return rel, nil
}

type sandbox struct {
	root *os.Root // nil when file access is disabled
}

func newSandbox(dir string) (*sandbox, error) {
	if dir == "" {
		return &sandbox{}, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open sandbox root: %w", err)
	}
	return &sandbox{root: root}, nil
}

func (s *sandbox) close() {
	if s.root != nil {
		s.root.Close()
	}
}

func isDevNull(p string) bool {
	return path.Clean(filepath.ToSlash(p)) == "/dev/null"
}

func denied(op, p string) error {
	return &fs.PathError{Op: op, Path: p, Err: ErrFileAccessDisabled}
}

func (s *sandbox) open(ctx context.Context, p string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if isDevNull(p) {
		return devNull{}, nil
	}
	if s.root == nil {
		return nil, denied("open", p)
	}
	rel, err := resolve(interp.HandlerCtx(ctx).Dir, p)
	if err != nil {
		return nil, err
	}
	return s.root.OpenFile(rel, flag, perm)
}

func (s *sandbox) stat(ctx context.Context, p string, followSymlinks bool) (fs.FileInfo, error) {
	if s.root == nil {
		// Let `cd /` and `pwd` work in a file-less shell.
		if path.Clean(filepath.ToSlash(p)) == "/" {
			return rootInfo{}, nil
		}
		return nil, denied("stat", p)
	}
	rel, err := resolve(interp.HandlerCtx(ctx).Dir, p)
	if err != nil {
		return nil, err
	}
	if followSymlinks {
		return s.root.Stat(rel)
	}
	return s.root.Lstat(rel)
}

func (s *sandbox) readDir(ctx context.Context, p string) ([]fs.DirEntry, error) {
	if s.root == nil {
		// No entries rather than an error: a glob then stays literal, as in bash, so an
		// unquoted pattern like `jq .[]` still works instead of failing the command.
		return nil, nil
	}
	rel, err := resolve(interp.HandlerCtx(ctx).Dir, p)
	if err != nil {
		return nil, err
	}
	return fs.ReadDir(s.root.FS(), rel)
}

func (s *sandbox) access(ctx context.Context, p string, mode interp.AccessMode) error {
	info, err := s.stat(ctx, p, true)
	if err != nil {
		return err
	}
	if s.root == nil {
		return nil // only "/" stats successfully
	}
	if mode&interp.AccessExec != 0 && !info.IsDir() {
		// Nothing in the sandbox is executable: commands come from the registry.
		return &fs.PathError{Op: "access", Path: p, Err: fs.ErrPermission}
	}
	perm := info.Mode().Perm()
	if mode&interp.AccessRead != 0 && perm&0o444 == 0 || mode&interp.AccessWrite != 0 && perm&0o222 == 0 {
		return &fs.PathError{Op: "access", Path: p, Err: fs.ErrPermission}
	}
	return nil
}

type devNull struct{}

func (devNull) Read([]byte) (int, error)    { return 0, io.EOF }
func (devNull) Write(b []byte) (int, error) { return len(b), nil }
func (devNull) Close() error                { return nil }

type rootInfo struct{}

func (rootInfo) Name() string       { return "/" }
func (rootInfo) Size() int64        { return 0 }
func (rootInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (rootInfo) ModTime() time.Time { return time.Time{} }
func (rootInfo) IsDir() bool        { return true }
func (rootInfo) Sys() any           { return nil }
