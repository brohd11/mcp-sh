package shengine_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brohd11/mcp-sh/builtins"
	"github.com/brohd11/mcp-sh/engine"
	"github.com/brohd11/mcp-sh/engine/shengine"
)

// upper is a stand-in host command: it upper-cases stdin and echoes its args.
var upper = engine.Command{
	Name: "upper", Summary: "upper-case stdin", Source: "host",
	Run: func(ctx context.Context, inv *engine.Invocation) int {
		b, _ := io.ReadAll(inv.Stdin)
		io.WriteString(inv.Stdout, strings.ToUpper(string(b)))
		for _, a := range inv.Args {
			io.WriteString(inv.Stdout, "["+a+"]")
		}
		return 0
	},
}

type result struct {
	out, errOut string
	code        int
	err         error
}

func run(t *testing.T, opts shengine.Options, script string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	reg := builtins.Registry([]engine.Command{upper}, nil)
	code, err := shengine.New(opts).Run(ctx, script, reg, engine.IO{Stdout: &out, Stderr: &errOut})
	return result{out.String(), errOut.String(), code, err}
}

func TestPipesAndLists(t *testing.T) {
	r := run(t, shengine.Options{}, `echo hi | upper && echo ok; false || echo recovered`)
	if r.out != "HI\nok\nrecovered\n" || r.code != 0 || r.err != nil {
		t.Fatalf("got %+v", r)
	}
}

func TestArgsAreWordsNotText(t *testing.T) {
	r := run(t, shengine.Options{}, `x="a b"; upper "$x" 'c d' e </dev/null`)
	if r.out != "[a b][c d][e]" {
		t.Fatalf("got %q", r.out)
	}
}

func TestExitStatus(t *testing.T) {
	if r := run(t, shengine.Options{}, `exit 3`); r.code != 3 || r.err != nil {
		t.Fatalf("exit 3: got %+v", r)
	}
	if r := run(t, shengine.Options{}, `false`); r.code != 1 {
		t.Fatalf("false: got %+v", r)
	}
}

func TestUnknownCommandsNeverExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /usr/bin/touch")
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	for _, script := range []string{
		`touch ` + marker,
		`/usr/bin/touch ` + marker,
		`command touch ` + marker,
		`exec /usr/bin/touch ` + marker,
		`eval "touch ` + marker + `"`,
		`PATH=/usr/bin:/bin touch ` + marker,
		`sh -c "touch ` + marker + `"`,
	} {
		r := run(t, shengine.Options{}, script)
		if r.code != 127 {
			t.Errorf("%q: exit %d, want 127 (stderr %q, err %v)", script, r.code, r.errOut, r.err)
		}
		if !strings.Contains(r.errOut, "command not found") {
			t.Errorf("%q: stderr %q", script, r.errOut)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%q created a file: an OS process ran", script)
		}
	}
}

func TestFileAccessDisabled(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	for _, script := range []string{
		`echo hi > ` + filepath.Join(t.TempDir(), "x"),
		`read -r line < ` + outside,
		`cat ` + outside,
		`source ` + outside,
		`cd ` + filepath.Dir(outside),
		`test -f ` + outside + ` && echo exists`,
	} {
		r := run(t, shengine.Options{}, script)
		if strings.Contains(r.out, "secret") || strings.Contains(r.out, "exists") {
			t.Errorf("%q leaked file data: %q", script, r.out)
		}
		if r.code == 0 {
			t.Errorf("%q succeeded; want failure (out %q)", script, r.out)
		}
	}
	// Globs see no files and stay literal, so unquoted jq filters still work.
	if r := run(t, shengine.Options{}, `echo * .[]; echo '[1]' | jq .[]`); r.out != "* .[]\n1\n" {
		t.Errorf("glob: got %+v", r)
	}
	// /dev/null and cd / still work.
	if r := run(t, shengine.Options{}, `echo hi >/dev/null; cd / && pwd`); r.out != "/\n" || r.code != 0 {
		t.Errorf("devnull/cd: got %+v", r)
	}
	// pwd prints the virtual directory; -P never resolves on the real filesystem.
	if r := run(t, shengine.Options{}, `pwd -P; pwd -x`); r.out != "/\n" || r.code != 2 {
		t.Errorf("pwd flags: got %+v", r)
	}
}

func TestRootJail(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	opts := shengine.Options{Root: root}

	r := run(t, opts, `echo hello > /sub/a.txt && cd sub && cat a.txt && pwd && echo *`)
	if r.out != "hello\n/sub\na.txt\n" || r.code != 0 {
		t.Fatalf("in-root work: got %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "sub", "a.txt")); string(b) != "hello\n" {
		t.Fatalf("file not written inside root: %q", b)
	}
	for _, script := range []string{
		`cat ../../../../../../` + strings.TrimPrefix(filepath.Join(outside, "secret.txt"), "/"),
		`cat /escape/secret.txt`,
		`cat < escape/secret.txt`,
		`echo x > /escape/new.txt`,
	} {
		r := run(t, opts, script)
		if strings.Contains(r.out, "secret") || r.code == 0 {
			t.Errorf("%q escaped the root: %+v", script, r)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("wrote through a symlink outside the root")
	}
}

func TestProcessSubstitutionRejected(t *testing.T) {
	r := run(t, shengine.Options{}, `cat <(echo hi)`)
	if r.err == nil || !strings.Contains(r.err.Error(), "process substitution") {
		t.Fatalf("got %+v", r)
	}
}

func TestSyntaxError(t *testing.T) {
	r := run(t, shengine.Options{}, `if then fi (`)
	if r.err == nil || r.code != 2 {
		t.Fatalf("got %+v", r)
	}
}

func TestTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	code, err := shengine.New(shengine.Options{}).Run(ctx, `while true; do :; done`, builtins.Registry(nil, nil), engine.IO{})
	if code != 124 || err == nil {
		t.Fatalf("got code %d err %v", code, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s to stop", time.Since(start))
	}
}

func TestEnvironmentIsolated(t *testing.T) {
	t.Setenv("MCP_SH_TEST_SECRET", "hunter2")
	r := run(t, shengine.Options{Env: []string{"FOO=bar"}}, `echo "$MCP_SH_TEST_SECRET|$FOO|$PATH"`)
	if r.out != "|bar|\n" {
		t.Fatalf("got %q", r.out)
	}
}

func TestHelpIsOurs(t *testing.T) {
	r := run(t, shengine.Options{}, `help | grep -c upper; help grep | head -1`)
	if !strings.HasPrefix(r.out, "1\nusage: grep") {
		t.Fatalf("got %q (stderr %q)", r.out, r.errOut)
	}
}

func TestShellFeatures(t *testing.T) {
	script := `
f() { echo "fn:$1"; }
for i in 1 2 3; do f "$i"; done | upper
n=$(echo abc | wc -c); echo "n=$n"
if [ "$n" -gt 2 ]; then echo big; fi
case x in x) echo case;; esac
arr=(a b c); echo "${#arr[@]} ${arr[1]}"
`
	r := run(t, shengine.Options{}, script)
	want := "FN:1\nFN:2\nFN:3\nn=4\nbig\ncase\n3 b\n"
	if r.out != want || r.code != 0 {
		t.Fatalf("got %q want %q (stderr %q)", r.out, want, r.errOut)
	}
}
