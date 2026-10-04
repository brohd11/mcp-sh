// Package builtins provides in-process utilities so scripts can filter host output
// without ever starting an OS process: cat, grep, head, tail, wc, sort, uniq, cut, tr,
// seq, sed (s/// only), jq, and help.
//
// They implement the common subset agents reach for, not full GNU behaviour; anything
// unsupported fails loudly with a usage error rather than being silently ignored.
package builtins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/brohd11/agent-shell/engine"
)

// All returns the generic builtins. help and host are built per run (see Registry).
func All() []engine.Command {
	return []engine.Command{
		catCmd, grepCmd, headCmd, tailCmd, wcCmd, sortCmd, uniqCmd,
		cutCmd, trCmd, seqCmd, sedCmd, jqCmd,
	}
}

// Registry builds the command set for one run. Precedence, lowest first: the generic
// builtins, the host's commands (the host knows its domain, e.g. a `cat` that reads
// project files), then extra, the host binary's own Go commands. Finally `host` (when
// there are host commands) and `help` over the result.
func Registry(hostCmds, extra []engine.Command) engine.Registry {
	reg := engine.Registry{}
	reg.Add(All()...)
	reg.Add(hostCmds...)
	reg.Add(extra...)
	if len(hostCmds) > 0 {
		reg.Add(Host(hostCmds))
	}
	reg.Add(Help(reg))
	return reg
}

// command wraps a body that reports usage errors as errUsage, so every builtin handles
// --help and bad flags the same way.
func command(name, summary, help string, body func(ctx context.Context, inv *engine.Invocation) error) engine.Command {
	return engine.Command{
		Name:    name,
		Summary: summary,
		Help:    help,
		Source:  "builtin",
		Run: func(ctx context.Context, inv *engine.Invocation) int {
			if len(inv.Args) == 1 && inv.Args[0] == "--help" {
				fmt.Fprintln(inv.Stdout, help)
				return 0
			}
			err := body(ctx, inv)
			var ex exitError
			switch {
			case err == nil:
				return 0
			case errors.As(err, &ex):
				return int(ex)
			case errors.Is(err, errUsage):
				fmt.Fprintf(inv.Stderr, "%s: %v\n%s\n", name, err, firstLine(help))
				return 2
			default:
				fmt.Fprintf(inv.Stderr, "%s: %v\n", name, err)
				return 1
			}
		},
	}
}

// exitError ends a builtin with a status and no message (grep with no match).
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

var errUsage = errors.New("usage error")

func usagef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, a...))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// flags is the result of parseFlags.
type flags struct {
	set  map[byte]bool
	vals map[byte][]string
	rest []string
}

func (f flags) has(c byte) bool { return f.set[c] }

func (f flags) last(c byte) (string, bool) {
	v := f.vals[c]
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

// parseFlags parses POSIX short options: bools are flag letters without a value, values
// are letters that take one ("-n5" or "-n 5"). Letters can be grouped ("-in"). longs maps
// long names to letters. With permute, options may follow operands, as GNU allows.
func parseFlags(args []string, bools, values string, longs map[string]byte, permute bool) (flags, error) {
	f := flags{set: map[byte]bool{}, vals: map[byte][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.rest = append(f.rest, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			name, val, hasVal := strings.Cut(a[2:], "=")
			c, ok := longs[name]
			if !ok {
				return f, usagef("unknown option %s", a)
			}
			if strings.IndexByte(values, c) >= 0 {
				if !hasVal {
					if i+1 >= len(args) {
						return f, usagef("option --%s needs a value", name)
					}
					i++
					val = args[i]
				}
				f.vals[c] = append(f.vals[c], val)
			}
			f.set[c] = true
			continue
		}
		if len(a) < 2 || a[0] != '-' {
			if !permute {
				f.rest = append(f.rest, args[i:]...)
				break
			}
			f.rest = append(f.rest, a)
			continue
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			switch {
			case strings.IndexByte(bools, c) >= 0:
				f.set[c] = true
			case strings.IndexByte(values, c) >= 0:
				val := a[j+1:]
				if val == "" {
					if i+1 >= len(args) {
						return f, usagef("option -%c needs a value", c)
					}
					i++
					val = args[i]
				}
				f.set[c] = true
				f.vals[c] = append(f.vals[c], val)
				j = len(a)
			default:
				return f, usagef("unknown option -%c", c)
			}
		}
	}
	return f, nil
}

// input is one named source: a file operand or stdin ("-").
type input struct {
	name string
	r    io.ReadCloser
}

// eachInput opens each operand in turn (stdin when there are none) and calls fn. An
// operand that cannot be opened is reported and skipped, and the result is then status 1.
func eachInput(inv *engine.Invocation, operands []string, fn func(in input) error) error {
	if len(operands) == 0 {
		operands = []string{"-"}
	}
	var firstErr error
	for _, name := range operands {
		r, err := openInput(inv, name)
		if err != nil {
			fmt.Fprintf(inv.Stderr, "%s: %v\n", name, err)
			if firstErr == nil {
				firstErr = exitError(1)
			}
			continue
		}
		err = fn(input{name: name, r: r})
		r.Close()
		if err != nil {
			return err
		}
	}
	return firstErr
}

func openInput(inv *engine.Invocation, name string) (io.ReadCloser, error) {
	if name == "-" {
		return io.NopCloser(inv.Stdin), nil
	}
	if inv.FS == nil {
		return nil, errors.New("file access is disabled in this shell; pipe data in instead")
	}
	rel, err := inv.Resolve(name)
	if err != nil {
		return nil, err
	}
	return inv.FS.Open(rel)
}

// maxLine bounds a single input line; hosts can emit large JSON on one line.
const maxLine = 64 << 20

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	return sc
}

// readLines reads every line of every input.
func readLines(inv *engine.Invocation, operands []string) ([]string, error) {
	var lines []string
	err := eachInput(inv, operands, func(in input) error {
		sc := newScanner(in.r)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		return sc.Err()
	})
	return lines, err
}
