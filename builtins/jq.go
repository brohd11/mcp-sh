package builtins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/itchyny/gojq"

	"github.com/brohd11/agent-shell/engine"
)

var jqCmd = command("jq", "filter JSON (gojq)",
	`usage: jq [-rjcsnRe] [--arg NAME VALUE] [--argjson NAME JSON] FILTER [file...]
Run a jq FILTER over each JSON value from stdin (or files).
  -r  raw strings          -j  raw, no newlines
  -c  compact output       -s  slurp all inputs into one array
  -n  null input           -R  raw input lines as strings
  -e  exit status from the last output (1 if false/null, 4 if none)
  --arg NAME VALUE / --argjson NAME JSON  bind $NAME
$ENV is empty and modules cannot be loaded.`,
	runJQ)

type jqOptions struct {
	raw, join, compact, slurp, nullInput, rawInput, exitStatus bool
	varNames                                                   []string
	varValues                                                  []any
	filter                                                     string
	files                                                      []string
}

func parseJQArgs(args []string) (jqOptions, error) {
	var o jqOptions
	longs := map[string]*bool{
		"raw-output": &o.raw, "join-output": &o.join, "compact-output": &o.compact,
		"slurp": &o.slurp, "null-input": &o.nullInput, "raw-input": &o.rawInput,
		"exit-status": &o.exitStatus,
	}
	shorts := map[byte]*bool{
		'r': &o.raw, 'j': &o.join, 'c': &o.compact, 's': &o.slurp,
		'n': &o.nullInput, 'R': &o.rawInput, 'e': &o.exitStatus,
	}
	var operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			operands = append(operands, args[i+1:]...)
			i = len(args)
		case a == "--arg" || a == "--argjson":
			if i+2 >= len(args) {
				return o, usagef("%s needs NAME and VALUE", a)
			}
			name, raw := args[i+1], args[i+2]
			var v any = raw
			if a == "--argjson" {
				dec := json.NewDecoder(strings.NewReader(raw))
				dec.UseNumber()
				if err := dec.Decode(&v); err != nil {
					return o, usagef("--argjson %s: invalid JSON: %v", name, err)
				}
			}
			o.varNames = append(o.varNames, "$"+name)
			o.varValues = append(o.varValues, v)
			i += 2
		case strings.HasPrefix(a, "--"):
			p, ok := longs[a[2:]]
			if !ok {
				return o, usagef("unknown option %s", a)
			}
			*p = true
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				p, ok := shorts[a[j]]
				if !ok {
					return o, usagef("unknown option -%c", a[j])
				}
				*p = true
			}
		default:
			operands = append(operands, a)
		}
	}
	if len(operands) == 0 {
		return o, usagef("missing filter")
	}
	o.filter, o.files = operands[0], operands[1:]
	if o.join {
		o.raw = true
	}
	return o, nil
}

func runJQ(ctx context.Context, inv *engine.Invocation) error {
	o, err := parseJQArgs(inv.Args)
	if err != nil {
		return err
	}
	query, err := gojq.Parse(o.filter)
	if err != nil {
		fmt.Fprintf(inv.Stderr, "jq: compile error: %v\n", err)
		return exitError(3)
	}

	data, err := readAllInputs(inv, o.files)
	if err != nil {
		return err
	}
	inputs := newJQInputs(data, o.rawInput, o.slurp)

	code, err := gojq.Compile(query,
		gojq.WithVariables(o.varNames),
		gojq.WithEnvironLoader(func() []string { return nil }), // never leak the server env
		gojq.WithInputIter(inputs),
	)
	if err != nil {
		fmt.Fprintf(inv.Stderr, "jq: compile error: %v\n", err)
		return exitError(3)
	}

	w := bufio.NewWriter(inv.Stdout)
	defer w.Flush()
	status := 0
	var last any
	outputs := 0
	run := func(v any) bool {
		iter := code.RunWithContext(ctx, v, o.varValues...)
		for {
			out, ok := iter.Next()
			if !ok {
				return true
			}
			if err, isErr := out.(error); isErr {
				var halt *gojq.HaltError
				if errors.As(err, &halt) {
					if hv := halt.Value(); hv != nil {
						w.Flush()
						if s, ok := hv.(string); ok {
							fmt.Fprint(inv.Stderr, s)
						} else {
							b, _ := gojq.Marshal(hv)
							fmt.Fprintln(inv.Stderr, string(b))
						}
					}
					status = halt.ExitCode()
					return false
				}
				w.Flush()
				fmt.Fprintf(inv.Stderr, "jq: error: %v\n", err)
				status = 5
				continue
			}
			outputs++
			last = out
			if err := writeJQ(w, out, o); err != nil {
				fmt.Fprintf(inv.Stderr, "jq: error: %v\n", err)
				status = 5
			}
		}
	}

	if o.nullInput {
		run(nil)
	} else {
		for {
			v, ok := inputs.Next()
			if !ok {
				break
			}
			if err, isErr := v.(error); isErr {
				w.Flush()
				fmt.Fprintf(inv.Stderr, "jq: error: %v\n", err)
				status = 2
				break
			}
			if !run(v) {
				break
			}
		}
	}
	if status == 0 && o.exitStatus {
		switch {
		case outputs == 0:
			status = 4
		case last == nil || last == false:
			status = 1
		}
	}
	if status != 0 {
		return exitError(status)
	}
	return nil
}

func readAllInputs(inv *engine.Invocation, files []string) ([]byte, error) {
	var buf bytes.Buffer
	err := eachInput(inv, files, func(in input) error {
		_, err := io.Copy(&buf, in.r)
		return err
	})
	return buf.Bytes(), err
}

func writeJQ(w *bufio.Writer, v any, o jqOptions) error {
	if s, ok := v.(string); ok && o.raw {
		w.WriteString(s)
	} else {
		b, err := gojq.Marshal(v)
		if err != nil {
			return err
		}
		if !o.compact {
			var ind bytes.Buffer
			if err := json.Indent(&ind, b, "", "  "); err == nil {
				b = ind.Bytes()
			}
		}
		w.Write(b)
	}
	if !o.join {
		w.WriteByte('\n')
	}
	return nil
}

// jqInputs yields the run's input values; it is shared by the main loop and the
// filter's `input`/`inputs` builtins, as in jq.
type jqInputs struct {
	values []any
	dec    *json.Decoder
	done   bool
}

func newJQInputs(data []byte, rawInput, slurp bool) *jqInputs {
	in := &jqInputs{}
	switch {
	case rawInput && slurp:
		in.values = []any{string(data)}
	case rawInput:
		text := strings.TrimSuffix(string(data), "\n")
		if text != "" || len(data) > 0 {
			for _, line := range strings.Split(text, "\n") {
				in.values = append(in.values, line)
			}
		}
	case slurp:
		dec := newJSONDecoder(data)
		all := []any{}
		for {
			var v any
			if err := dec.Decode(&v); err == io.EOF {
				break
			} else if err != nil {
				in.values = []any{err}
				return in
			}
			all = append(all, v)
		}
		in.values = []any{all}
	default:
		in.dec = newJSONDecoder(data)
	}
	return in
}

func newJSONDecoder(data []byte) *json.Decoder {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // keep integer precision; gojq handles json.Number
	return dec
}

func (in *jqInputs) Next() (any, bool) {
	if in.done {
		return nil, false
	}
	if in.dec == nil {
		if len(in.values) == 0 {
			in.done = true
			return nil, false
		}
		v := in.values[0]
		in.values = in.values[1:]
		return v, true
	}
	var v any
	if err := in.dec.Decode(&v); err != nil {
		in.done = true
		if err == io.EOF {
			return nil, false
		}
		return fmt.Errorf("invalid JSON input: %w", err), true
	}
	return v, true
}
