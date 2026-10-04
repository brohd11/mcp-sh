package builtins_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brohd11/agent-shell/builtins"
	"github.com/brohd11/agent-shell/engine"
	"github.com/brohd11/agent-shell/engine/shengine"
)

func sh(t *testing.T, root, script string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	code, err := shengine.New(shengine.Options{Root: root}).Run(ctx, script, builtins.Registry(nil, nil), engine.IO{Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("%q: %v", script, err)
	}
	return out.String(), errOut.String(), code
}

const fruit = `printf 'banana 3\napple 10\ncherry 2\napple 10\nBanana 1\n'`

func TestBuiltins(t *testing.T) {
	cases := []struct {
		name, script, want string
		code               int
	}{
		{"cat", `echo hi | cat`, "hi\n", 0},

		{"grep", fruit + ` | grep an`, "banana 3\nBanana 1\n", 0},
		{"grep -i", fruit + ` | grep -i ^ban`, "banana 3\nBanana 1\n", 0},
		{"grep -v -c", fruit + ` | grep -vc apple`, "3\n", 0},
		{"grep -n", fruit + ` | grep -n cherry`, "3:cherry 2\n", 0},
		{"grep -o", `echo a1b22c333 | grep -oE '[0-9]+'`, "1\n22\n333\n", 0},
		{"grep -w", `printf 'cat\ncatalog\n' | grep -w cat`, "cat\n", 0},
		{"grep -x", `printf 'cat\ncatalog\n' | grep -x 'cat.*g'`, "catalog\n", 0},
		{"grep -F", `printf 'a.c\nabc\n' | grep -F a.c`, "a.c\n", 0},
		{"grep BRE alternation", fruit + ` | grep 'cherry\|Banana'`, "cherry 2\nBanana 1\n", 0},
		{"grep BRE literal parens", `printf 'f(x)\nfx\n' | grep 'f(x)'`, "f(x)\n", 0},
		{"grep -e multiple", fruit + ` | grep -e cherry -e Ban`, "cherry 2\nBanana 1\n", 0},
		{"grep flag after pattern", fruit + ` | grep banana -i`, "banana 3\nBanana 1\n", 0},
		{"grep -m", fruit + ` | grep -m1 a`, "banana 3\n", 0},
		{"grep no match", `echo x | grep y`, "", 1},
		{"grep -q", `echo x | grep -q x && echo yes`, "yes\n", 0},

		{"head", `seq 20 | head -3`, "1\n2\n3\n", 0},
		{"head shorthand", `seq 20 | head -2`, "1\n2\n", 0},
		{"head all but", `seq 5 | head -n -3`, "1\n2\n", 0},
		{"head default", `seq 20 | head | wc -l`, "10\n", 0},
		{"tail", `seq 20 | tail -n 2`, "19\n20\n", 0},
		{"tail from", `seq 5 | tail -n +4`, "4\n5\n", 0},

		{"wc", `printf 'a b\nc\n' | wc`, "2 3 6\n", 0},
		{"wc -l", `printf 'a b\nc\n' | wc -l`, "2\n", 0},
		{"wc -w", `echo one two three | wc -w`, "3\n", 0},

		{"sort", fruit + ` | sort`, "Banana 1\napple 10\napple 10\nbanana 3\ncherry 2\n", 0},
		{"sort -f -u", fruit + ` | sort -fu | cut -d' ' -f1`, "apple\nBanana\nbanana\ncherry\n", 0},
		{"sort -k2 -n", fruit + ` | sort -k2 -n | cut -d' ' -f1`, "Banana\ncherry\nbanana\napple\napple\n", 0},
		{"sort -k2nr", fruit + ` | sort -k2nr | head -1`, "apple 10\n", 0},
		{"sort -t", `printf 'b:2\na:1\n' | sort -t: -k2`, "a:1\nb:2\n", 0},

		{"uniq", fruit + ` | sort | uniq | wc -l`, "4\n", 0},
		{"uniq -c", `printf 'a\na\nb\n' | uniq -c`, "      2 a\n      1 b\n", 0},
		{"uniq -d", `printf 'a\na\nb\n' | uniq -d`, "a\n", 0},

		{"cut -f", `printf 'a\tb\tc\n' | cut -f2,3`, "b\tc\n", 0},
		{"cut -d range", `echo a,b,c,d | cut -d, -f2-`, "b,c,d\n", 0},
		{"cut -c", `echo abcdef | cut -c1-3`, "abc\n", 0},

		{"tr", `echo hello | tr a-z A-Z`, "HELLO\n", 0},
		{"tr classes", `echo Hello | tr '[:upper:]' '[:lower:]'`, "hello\n", 0},
		{"tr -d", `echo h-e-l-l-o | tr -d -`, "hello\n", 0},
		{"tr -s", `echo 'a   b' | tr -s ' '`, "a b\n", 0},
		{"tr newline", `printf 'a,b\n' | tr , '\n'`, "a\nb\n", 0},

		{"seq", `seq 3`, "1\n2\n3\n", 0},
		{"seq step", `seq 1 2 7`, "1\n3\n5\n7\n", 0},
		{"seq down", `seq 3 -1 1`, "3\n2\n1\n", 0},
		{"seq -s", `seq -s, 3`, "1,2,3\n", 0},

		{"sed", `echo hello world | sed 's/o/0/'`, "hell0 world\n", 0},
		{"sed g", `echo hello world | sed 's/o/0/g'`, "hell0 w0rld\n", 0},
		{"sed groups", `echo 'key=value' | sed -E 's/(\w+)=(\w+)/\2=\1/'`, "value=key\n", 0},
		{"sed BRE groups", `echo 'key=value' | sed 's/\(.*\)=\(.*\)/\2 &/'`, "value key=value\n", 0},
		{"sed delimiter", `echo /a/b | sed 's|/|.|g'`, ".a.b\n", 0},
		{"sed -n p", `printf 'a\nb\n' | sed -n 's/b/B/p'`, "B\n", 0},
		{"sed multiple", `echo abc | sed -e 's/a/1/' -e 's/c/3/'`, "1b3\n", 0},
		{"sed nth", `echo aaa | sed 's/a/b/2'`, "aba\n", 0},
		{"sed i flag", `echo ABC | sed 's/b/x/i'`, "AxC\n", 0},

		{"jq", `echo '{"a":{"b":[1,2]}}' | jq -c .a`, `{"b":[1,2]}` + "\n", 0},
		{"jq pretty", `echo '{"a":1}' | jq .`, "{\n  \"a\": 1\n}\n", 0},
		{"jq -r", `echo '[{"n":"x"},{"n":"y"}]' | jq -r '.[].n'`, "x\ny\n", 0},
		{"jq stream", `printf '{"n":1}\n{"n":2}\n' | jq .n`, "1\n2\n", 0},
		{"jq -s", `printf '1 2 3' | jq -s add`, "6\n", 0},
		{"jq -n --arg", `jq -n --arg x hi '$x'`, "\"hi\"\n", 0},
		{"jq --argjson", `jq -nc --argjson x '{"a":1}' '$x.a'`, "1\n", 0},
		{"jq -R", `printf 'a\nb\n' | jq -R .`, "\"a\"\n\"b\"\n", 0},
		{"jq big int", `echo 12345678901234567890 | jq .`, "12345678901234567890\n", 0},
		{"jq select", `echo '[1,5,10]' | jq -c 'map(select(. > 3))'`, "[5,10]\n", 0},
		{"jq inputs", `printf '1\n2\n3\n' | jq -n '[inputs] | add'`, "6\n", 0},
		{"jq -e false", `echo false | jq -e .`, "false\n", 1},
		{"jq no ENV", `jq -n '$ENV | length'`, "0\n", 0},
		{"jq env builtin", `jq -n 'env | length'`, "0\n", 0},
		{"jq -j", `echo '["a","b"]' | jq -j '.[]'`, "ab", 0},
		{"jq unicode", `echo '"<é>"' | jq .`, "\"<é>\"\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errOut, code := sh(t, "", c.script)
			if out != c.want || code != c.code {
				t.Errorf("%s\n got %q (exit %d, stderr %q)\nwant %q (exit %d)", c.script, out, code, errOut, c.want, c.code)
			}
		})
	}
}

func TestBuiltinErrors(t *testing.T) {
	cases := []struct {
		script, stderr string
		code           int
	}{
		{`echo x | grep -Z x`, "unknown option -Z", 2},
		{`echo x | head -n abc`, "invalid line count", 2},
		{`echo x | sed '1d'`, "only s/// is supported", 2},
		{`echo x | sed 's/x/y'`, "unterminated", 2},
		{`echo '{' | jq .`, "invalid JSON", 2},
		{`jq -n '.['`, "compile error", 3},
		{`jq -n 'error("boom")'`, "boom", 5},
		{`cat /etc/passwd`, "file access is disabled", 1},
		{`grep -r x .`, "not supported", 2},
		{`cut -f1 -c1`, "exactly one of", 2},
	}
	for _, c := range cases {
		_, errOut, code := sh(t, "", c.script)
		if code != c.code || !strings.Contains(errOut, c.stderr) {
			t.Errorf("%s\n got exit %d stderr %q\nwant exit %d containing %q", c.script, code, errOut, c.code, c.stderr)
		}
	}
}

func TestHelpFlag(t *testing.T) {
	for _, cmd := range builtins.All() {
		out, _, code := sh(t, "", cmd.Name+" --help")
		if code != 0 || !strings.HasPrefix(out, "usage: "+cmd.Name) {
			t.Errorf("%s --help: exit %d, %q", cmd.Name, code, out)
		}
	}
}

func TestFileOperands(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.json"), []byte(`{"x":1}`), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("one\ntwo\n"), 0o644)
	out, errOut, code := sh(t, root, `jq .x a.json; grep -c o b.txt /b.txt; cat missing.txt`)
	want := "1\nb.txt:2\n/b.txt:2\n"
	if out != want || code != 1 || !strings.Contains(errOut, "missing.txt") {
		t.Fatalf("got %q exit %d stderr %q", out, code, errOut)
	}
}

func TestHeadClosesPipeEarly(t *testing.T) {
	// A Go command upstream of head sees EPIPE and stops. (A pure-shell loop does not:
	// the interpreter has no SIGPIPE, so it runs until it finishes or times out.)
	out, _, code := sh(t, "", `seq 1000000 | head -2`)
	if out != "1\n2\n" || code != 0 {
		t.Fatalf("got %q exit %d", out, code)
	}
	out, _, _ = sh(t, "", `set -o pipefail; seq 1000000 | head -1; echo "status=$?"`)
	if out != "1\nstatus=141\n" {
		t.Fatalf("pipefail: got %q", out)
	}
}

func TestHelpList(t *testing.T) {
	reg := builtins.Registry([]engine.Command{
		{Name: "scene", Summary: "inspect the scene", Source: "host"},
		{Name: "cat", Summary: "print a project file", Source: "host"},
	}, nil)
	list := builtins.FormatList(reg)
	if !strings.HasPrefix(list, "Host commands:\n  cat    print a project file (replaces the builtin cat)\n  scene  inspect the scene\n\nBuiltins") {
		t.Fatalf("got:\n%s", list)
	}
	if !strings.Contains(list, "  host ") {
		t.Fatalf("no host builtin:\n%s", list)
	}
}
