package builtins

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/brohd11/mcp-sh/engine"
)

var catCmd = command("cat", "concatenate input to stdout",
	"usage: cat [file...]\nCopy stdin (or files, when file access is enabled) to stdout.",
	func(ctx context.Context, inv *engine.Invocation) error {
		f, err := parseFlags(inv.Args, "", "", nil, true)
		if err != nil {
			return err
		}
		return eachInput(inv, f.rest, func(in input) error {
			_, err := io.Copy(inv.Stdout, in.r)
			return err
		})
	})

// ---- grep ----

var grepCmd = command("grep", "print lines matching a pattern",
	`usage: grep [-ivcnoqwxlEFHh] [-m NUM] [-e PATTERN]... [PATTERN] [file...]
Print lines matching PATTERN (Go RE2 syntax; basic-regex \| \( \) \{ \} \+ \? also work).
  -i  ignore case            -v  select non-matching lines
  -c  print only a count      -n  prefix line numbers
  -o  print only the matches  -q  quiet; exit status only
  -w  match whole words       -x  match whole lines
  -l  print names of matching files
  -E  extended regex (ERE)    -F  fixed strings
  -H/-h  force/suppress file name prefixes
  -m NUM  stop after NUM matching lines
  -e PATTERN  pattern (repeatable)
Exit status: 0 if a line matched, 1 if none, 2 on error.`,
	runGrep)

func runGrep(ctx context.Context, inv *engine.Invocation) error {
	f, err := parseFlags(inv.Args, "ivcnoqwxlEFHhr", "em", map[string]byte{
		"ignore-case": 'i', "invert-match": 'v', "count": 'c', "line-number": 'n',
		"only-matching": 'o', "quiet": 'q', "silent": 'q', "word-regexp": 'w', "line-regexp": 'x',
		"files-with-matches": 'l', "extended-regexp": 'E', "fixed-strings": 'F',
		"with-filename": 'H', "no-filename": 'h', "regexp": 'e', "max-count": 'm',
	}, true)
	if err != nil {
		return err
	}
	if f.has('r') {
		return usagef("-r is not supported; there is no directory tree to search")
	}
	patterns := f.vals['e']
	operands := f.rest
	if len(patterns) == 0 {
		if len(operands) == 0 {
			return usagef("missing pattern")
		}
		patterns, operands = operands[:1], operands[1:]
	}
	re, err := compileGrep(patterns, f.has('E'), f.has('F'), f.has('i'), f.has('w'), f.has('x'))
	if err != nil {
		return usagef("bad pattern: %v", err)
	}
	maxCount := -1
	if v, ok := f.last('m'); ok {
		if maxCount, err = strconv.Atoi(v); err != nil || maxCount < 0 {
			return usagef("invalid max count %q", v)
		}
	}
	showName := (len(operands) > 1 || f.has('H')) && !f.has('h')
	invert := f.has('v')
	matchedAny := false

	err = eachInput(inv, operands, func(in input) error {
		name := in.name
		if name == "-" {
			name = "(standard input)"
		}
		prefix := ""
		if showName {
			prefix = name + ":"
		}
		count := 0
		sc := newScanner(in.r)
		for lineNo := 1; sc.Scan(); lineNo++ {
			if maxCount >= 0 && count >= maxCount {
				break
			}
			line := sc.Text()
			if re.MatchString(line) == invert {
				continue
			}
			count++
			matchedAny = true
			switch {
			case f.has('q'):
				return exitError(0)
			case f.has('l'):
				fmt.Fprintln(inv.Stdout, name)
				return nil
			case f.has('c'):
			case f.has('o') && !invert:
				for _, m := range re.FindAllString(line, -1) {
					if m != "" {
						fmt.Fprintf(inv.Stdout, "%s%s%s\n", prefix, numPrefix(f.has('n'), lineNo), m)
					}
				}
			default:
				fmt.Fprintf(inv.Stdout, "%s%s%s\n", prefix, numPrefix(f.has('n'), lineNo), line)
			}
		}
		if f.has('c') {
			fmt.Fprintf(inv.Stdout, "%s%d\n", prefix, count)
		}
		return sc.Err()
	})
	if err != nil {
		return err
	}
	if !matchedAny {
		return exitError(1)
	}
	return nil
}

func numPrefix(on bool, n int) string {
	if !on {
		return ""
	}
	return strconv.Itoa(n) + ":"
}

func compileGrep(patterns []string, extended, fixed, icase, word, whole bool) (*regexp.Regexp, error) {
	var alts []string
	for _, p := range patterns {
		// As in grep, a newline separates patterns.
		for _, part := range strings.Split(p, "\n") {
			switch {
			case fixed:
				part = regexp.QuoteMeta(part)
			case !extended:
				part = breToERE(part)
			}
			alts = append(alts, "(?:"+part+")")
		}
	}
	expr := strings.Join(alts, "|")
	if word {
		expr = `\b(?:` + expr + `)\b`
	}
	if whole {
		expr = `^(?:` + expr + `)$`
	}
	if icase {
		expr = "(?i)" + expr
	}
	return regexp.Compile(expr)
}

// breToERE converts GNU basic-regex syntax to RE2: in BRE, \| \( \) \{ \} \+ \? are
// operators and the bare characters are literals.
func breToERE(p string) string {
	const ops = "|(){}+?"
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' && i+1 < len(p) {
			next := p[i+1]
			if strings.IndexByte(ops, next) >= 0 {
				b.WriteByte(next)
			} else {
				b.WriteByte(c)
				b.WriteByte(next)
			}
			i++
			continue
		}
		if strings.IndexByte(ops, c) >= 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ---- head / tail ----

// countShorthand rewrites a leading "-5" into "-n 5".
func countShorthand(args []string) []string {
	if len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		if _, err := strconv.Atoi(args[0][1:]); err == nil {
			return append([]string{"-n", args[0][1:]}, args[1:]...)
		}
	}
	return args
}

var headCmd = command("head", "print the first lines of input",
	"usage: head [-n NUM] [file...]\nPrint the first NUM lines (default 10). -n -NUM prints all but the last NUM.",
	func(ctx context.Context, inv *engine.Invocation) error {
		f, err := parseFlags(countShorthand(inv.Args), "", "n", map[string]byte{"lines": 'n'}, true)
		if err != nil {
			return err
		}
		n, allBut := 10, false
		if v, ok := f.last('n'); ok {
			allBut = strings.HasPrefix(v, "-")
			if n, err = strconv.Atoi(strings.TrimPrefix(v, "-")); err != nil || n < 0 {
				return usagef("invalid line count %q", v)
			}
		}
		return eachInput(inv, f.rest, func(in input) error {
			if len(f.rest) > 1 {
				fmt.Fprintf(inv.Stdout, "==> %s <==\n", in.name)
			}
			sc := newScanner(in.r)
			if allBut {
				var lines []string
				for sc.Scan() {
					lines = append(lines, sc.Text())
				}
				for i := 0; i < len(lines)-n; i++ {
					fmt.Fprintln(inv.Stdout, lines[i])
				}
				return sc.Err()
			}
			// Stop early, like head(1): the shell then closes the pipe and the
			// upstream writer sees EPIPE instead of blocking.
			for i := 0; i < n && sc.Scan(); i++ {
				fmt.Fprintln(inv.Stdout, sc.Text())
			}
			return sc.Err()
		})
	})

var tailCmd = command("tail", "print the last lines of input",
	"usage: tail [-n NUM | -n +NUM] [file...]\nPrint the last NUM lines (default 10), or from line NUM onwards with +NUM.",
	func(ctx context.Context, inv *engine.Invocation) error {
		f, err := parseFlags(countShorthand(inv.Args), "", "n", map[string]byte{"lines": 'n'}, true)
		if err != nil {
			return err
		}
		n, from := 10, false
		if v, ok := f.last('n'); ok {
			from = strings.HasPrefix(v, "+")
			if n, err = strconv.Atoi(strings.TrimLeft(v, "+-")); err != nil || n < 0 {
				return usagef("invalid line count %q", v)
			}
		}
		return eachInput(inv, f.rest, func(in input) error {
			if len(f.rest) > 1 {
				fmt.Fprintf(inv.Stdout, "==> %s <==\n", in.name)
			}
			sc := newScanner(in.r)
			var lines []string
			for lineNo := 1; sc.Scan(); lineNo++ {
				if from {
					if lineNo >= n {
						fmt.Fprintln(inv.Stdout, sc.Text())
					}
					continue
				}
				lines = append(lines, sc.Text())
				if len(lines) > n {
					lines = lines[1:]
				}
			}
			for _, l := range lines {
				fmt.Fprintln(inv.Stdout, l)
			}
			return sc.Err()
		})
	})

// ---- wc ----

var wcCmd = command("wc", "count lines, words and bytes",
	"usage: wc [-lwcm] [file...]\nPrint line, word and byte counts (-l lines, -w words, -c bytes, -m characters).",
	func(ctx context.Context, inv *engine.Invocation) error {
		f, err := parseFlags(inv.Args, "lwcm", "", map[string]byte{"lines": 'l', "words": 'w', "bytes": 'c', "chars": 'm'}, true)
		if err != nil {
			return err
		}
		which := []byte{}
		for _, c := range []byte("lwmc") {
			if f.has(c) {
				which = append(which, c)
			}
		}
		if len(which) == 0 {
			which = []byte("lwc")
		}
		total := map[byte]int{}
		print := func(counts map[byte]int, name string) {
			parts := make([]string, 0, len(which)+1)
			for _, c := range which {
				parts = append(parts, strconv.Itoa(counts[c]))
			}
			if name != "" {
				parts = append(parts, name)
			}
			fmt.Fprintln(inv.Stdout, strings.Join(parts, " "))
		}
		err = eachInput(inv, f.rest, func(in input) error {
			data, err := io.ReadAll(in.r)
			if err != nil {
				return err
			}
			counts := map[byte]int{
				'l': strings.Count(string(data), "\n"),
				'w': len(strings.Fields(string(data))),
				'c': len(data),
				'm': utf8.RuneCount(data),
			}
			for k, v := range counts {
				total[k] += v
			}
			name := ""
			if len(f.rest) > 0 {
				name = in.name
			}
			print(counts, name)
			return nil
		})
		if len(f.rest) > 1 {
			print(total, "total")
		}
		return err
	})

// ---- sort / uniq ----

var sortCmd = command("sort", "sort lines",
	`usage: sort [-rnuf] [-t SEP] [-k FIELD[,FIELD]] [file...]
  -r  reverse         -n  numeric (leading number)
  -u  unique lines    -f  ignore case
  -t SEP    field separator (default: runs of blanks)
  -k F[,G]  sort on fields F..G (1-based; default F to end of line)`,
	func(ctx context.Context, inv *engine.Invocation) error {
		f, err := parseFlags(inv.Args, "rnuf", "tk", map[string]byte{
			"reverse": 'r', "numeric-sort": 'n', "unique": 'u', "ignore-case": 'f',
			"field-separator": 't', "key": 'k',
		}, true)
		if err != nil {
			return err
		}
		numeric, reverse := f.has('n'), f.has('r')
		sep, _ := f.last('t')
		from, to := 0, 0
		if k, ok := f.last('k'); ok {
			if from, to, err = parseKey(k, &numeric, &reverse); err != nil {
				return err
			}
		}
		lines, err := readLines(inv, f.rest)
		if err != nil {
			return err
		}
		key := func(line string) string {
			if from > 0 {
				line = fieldRange(line, sep, from, to)
			}
			if f.has('f') {
				line = strings.ToLower(line)
			}
			return line
		}
		less := func(a, b string) int {
			ka, kb := key(a), key(b)
			if numeric {
				na, nb := leadingNumber(ka), leadingNumber(kb)
				switch {
				case na < nb:
					return -1
				case na > nb:
					return 1
				}
				return 0
			}
			return strings.Compare(ka, kb)
		}
		sort.SliceStable(lines, func(i, j int) bool {
			c := less(lines[i], lines[j])
			if reverse {
				return c > 0
			}
			return c < 0
		})
		var prev string
		for i, l := range lines {
			if f.has('u') && i > 0 && less(prev, l) == 0 {
				continue
			}
			prev = l
			fmt.Fprintln(inv.Stdout, l)
		}
		return nil
	})

// parseKey parses "2", "2,3", "2n", "2,2nr" into a 1-based field range. Per-key n/r
// options are applied globally.
func parseKey(k string, numeric, reverse *bool) (int, int, error) {
	parseOne := func(s string) (int, error) {
		digits := strings.TrimRightFunc(s, unicode.IsLetter)
		for _, c := range s[len(digits):] {
			switch c {
			case 'n':
				*numeric = true
			case 'r':
				*reverse = true
			default:
				return 0, usagef("unsupported key option %q", c)
			}
		}
		digits, _, _ = strings.Cut(digits, ".") // character offsets are ignored
		n, err := strconv.Atoi(digits)
		if err != nil || n < 1 {
			return 0, usagef("invalid key %q", k)
		}
		return n, nil
	}
	a, b, hasEnd := strings.Cut(k, ",")
	from, err := parseOne(a)
	if err != nil || !hasEnd {
		return from, 0, err
	}
	to, err := parseOne(b)
	return from, to, err
}

func splitFields(line, sep string) []string {
	if sep == "" {
		return strings.Fields(line)
	}
	return strings.Split(line, sep)
}

// fieldRange returns fields from..to (1-based, to==0 means to the end), re-joined.
func fieldRange(line, sep string, from, to int) string {
	fields := splitFields(line, sep)
	if from > len(fields) {
		return ""
	}
	if to == 0 || to > len(fields) {
		to = len(fields)
	}
	if to < from {
		return ""
	}
	joiner := sep
	if joiner == "" {
		joiner = " "
	}
	return strings.Join(fields[from-1:to], joiner)
}

var leadingNumberRe = regexp.MustCompile(`^\s*[-+]?(\d+\.?\d*|\.\d+)([eE][-+]?\d+)?`)

func leadingNumber(s string) float64 {
	m := leadingNumberRe.FindString(s)
	if m == "" {
		return 0
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(m), 64)
	if err != nil {
		return 0
	}
	return n
}

var uniqCmd = command("uniq", "collapse adjacent duplicate lines",
	"usage: uniq [-cdui] [file]\n  -c  prefix counts   -d  only duplicated lines\n  -u  only unique lines  -i  ignore case",
	func(ctx context.Context, inv *engine.Invocation) error {
		f, err := parseFlags(inv.Args, "cdui", "", map[string]byte{
			"count": 'c', "repeated": 'd', "unique": 'u', "ignore-case": 'i',
		}, true)
		if err != nil {
			return err
		}
		if len(f.rest) > 1 {
			return usagef("an output file operand is not supported")
		}
		lines, err := readLines(inv, f.rest)
		if err != nil {
			return err
		}
		same := func(a, b string) bool {
			if f.has('i') {
				return strings.EqualFold(a, b)
			}
			return a == b
		}
		for i := 0; i < len(lines); {
			j := i + 1
			for j < len(lines) && same(lines[i], lines[j]) {
				j++
			}
			n := j - i
			if (!f.has('d') || n > 1) && (!f.has('u') || n == 1) {
				if f.has('c') {
					fmt.Fprintf(inv.Stdout, "%7d %s\n", n, lines[i])
				} else {
					fmt.Fprintln(inv.Stdout, lines[i])
				}
			}
			i = j
		}
		return nil
	})

// ---- cut ----

var cutCmd = command("cut", "select fields or characters from each line",
	"usage: cut -f LIST [-d DELIM] [-s] [file...] | cut -c LIST [file...]\nLIST is comma-separated N, N-M, N- or -M (1-based). DELIM defaults to tab.",
	func(ctx context.Context, inv *engine.Invocation) error {
		f, err := parseFlags(inv.Args, "s", "dfc", map[string]byte{
			"delimiter": 'd', "fields": 'f', "characters": 'c', "only-delimited": 's',
		}, true)
		if err != nil {
			return err
		}
		fieldList, byField := f.last('f')
		charList, byChar := f.last('c')
		if byField == byChar {
			return usagef("specify exactly one of -f or -c")
		}
		list := fieldList
		if byChar {
			list = charList
		}
		ranges, err := parseList(list)
		if err != nil {
			return err
		}
		delim := "\t"
		if d, ok := f.last('d'); ok {
			if utf8.RuneCountInString(d) != 1 {
				return usagef("the delimiter must be a single character")
			}
			delim = d
		}
		return eachInput(inv, f.rest, func(in input) error {
			sc := newScanner(in.r)
			for sc.Scan() {
				line := sc.Text()
				if byChar {
					runes := []rune(line)
					var b strings.Builder
					for i, r := range runes {
						if inRanges(ranges, i+1) {
							b.WriteRune(r)
						}
					}
					fmt.Fprintln(inv.Stdout, b.String())
					continue
				}
				if !strings.Contains(line, delim) {
					if !f.has('s') {
						fmt.Fprintln(inv.Stdout, line)
					}
					continue
				}
				fields := strings.Split(line, delim)
				var out []string
				for i, field := range fields {
					if inRanges(ranges, i+1) {
						out = append(out, field)
					}
				}
				fmt.Fprintln(inv.Stdout, strings.Join(out, delim))
			}
			return sc.Err()
		})
	})

type span struct{ lo, hi int } // inclusive, 1-based; hi==0 means open-ended

func parseList(list string) ([]span, error) {
	var out []span
	for _, part := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		var s span
		var err error
		if lo != "" {
			if s.lo, err = strconv.Atoi(lo); err != nil || s.lo < 1 {
				return nil, usagef("invalid list %q", list)
			}
		} else {
			s.lo = 1
		}
		switch {
		case !isRange:
			s.hi = s.lo
		case hi != "":
			if s.hi, err = strconv.Atoi(hi); err != nil || s.hi < s.lo {
				return nil, usagef("invalid list %q", list)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func inRanges(spans []span, n int) bool {
	for _, s := range spans {
		if n >= s.lo && (s.hi == 0 || n <= s.hi) {
			return true
		}
	}
	return false
}

// ---- tr ----

var trCmd = command("tr", "translate or delete characters",
	`usage: tr [-ds] SET1 [SET2]
Translate characters in SET1 to SET2; -d deletes SET1; -s squeezes repeats of the
result set. Sets support ranges (a-z), escapes (\n \t \\) and [:upper:] [:lower:]
[:digit:] [:alpha:] [:alnum:] [:space:] [:punct:].`,
	func(ctx context.Context, inv *engine.Invocation) error {
		f, err := parseFlags(inv.Args, "ds", "", map[string]byte{"delete": 'd', "squeeze-repeats": 's'}, false)
		if err != nil {
			return err
		}
		var set1, set2 []rune
		switch {
		case len(f.rest) == 1 && (f.has('d') || f.has('s')):
		case len(f.rest) == 2 && !f.has('d'):
		default:
			return usagef("wrong number of sets")
		}
		if set1, err = expandSet(f.rest[0]); err != nil {
			return err
		}
		if len(f.rest) == 2 {
			if set2, err = expandSet(f.rest[1]); err != nil {
				return err
			}
			if len(set2) == 0 {
				return usagef("SET2 is empty")
			}
		}
		mapping := map[rune]rune{}
		deleteSet := map[rune]bool{}
		for i, r := range set1 {
			if f.has('d') {
				deleteSet[r] = true
			} else if set2 != nil {
				mapping[r] = set2[min(i, len(set2)-1)]
			}
		}
		squeeze := map[rune]bool{}
		if f.has('s') {
			src := set2
			if src == nil {
				src = set1
			}
			for _, r := range src {
				squeeze[r] = true
			}
		}
		data, err := io.ReadAll(inv.Stdin)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(inv.Stdout)
		last, hasLast := rune(0), false
		for _, r := range string(data) {
			if deleteSet[r] {
				continue
			}
			if m, ok := mapping[r]; ok {
				r = m
			}
			if squeeze[r] && hasLast && last == r {
				continue
			}
			w.WriteRune(r)
			last, hasLast = r, true
		}
		return w.Flush()
	})

var charClasses = map[string]func(rune) bool{
	"upper": unicode.IsUpper, "lower": unicode.IsLower, "digit": unicode.IsDigit,
	"alpha": unicode.IsLetter, "space": unicode.IsSpace, "punct": unicode.IsPunct,
	"alnum": func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) },
}

// expandSet expands a tr set. Character classes cover ASCII only, so [:upper:] and
// [:lower:] line up for case conversion.
func expandSet(s string) ([]rune, error) {
	var out []rune
	in := []rune(s)
	for i := 0; i < len(in); i++ {
		if in[i] == '[' && i+1 < len(in) && in[i+1] == ':' {
			end := strings.Index(string(in[i:]), ":]")
			if end > 0 {
				name := string(in[i+2 : i+end])
				pred, ok := charClasses[name]
				if !ok {
					return nil, usagef("unknown class [:%s:]", name)
				}
				for r := rune(0); r < 128; r++ {
					if pred(r) {
						out = append(out, r)
					}
				}
				i += end + 1
				continue
			}
		}
		r := in[i]
		if r == '\\' && i+1 < len(in) {
			i++
			switch in[i] {
			case 'n':
				r = '\n'
			case 't':
				r = '\t'
			case 'r':
				r = '\r'
			default:
				r = in[i]
			}
		}
		if i+2 < len(in) && in[i+1] == '-' {
			hi := in[i+2]
			if hi < r {
				return nil, usagef("range %c-%c is reversed", r, hi)
			}
			for c := r; c <= hi; c++ {
				out = append(out, c)
			}
			i += 2
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// ---- seq ----

var seqCmd = command("seq", "print a sequence of numbers",
	"usage: seq [-s SEP] [FIRST [INCREMENT]] LAST\nPrint numbers from FIRST (default 1) to LAST by INCREMENT (default 1).",
	func(ctx context.Context, inv *engine.Invocation) error {
		// Not permuted, so negative operands like "seq 5 -1 1" are numbers, not options.
		f, err := parseFlags(inv.Args, "", "s", map[string]byte{"separator": 's'}, false)
		if err != nil {
			return err
		}
		nums := make([]float64, len(f.rest))
		isFloat := false
		for i, a := range f.rest {
			if nums[i], err = strconv.ParseFloat(a, 64); err != nil {
				return usagef("invalid number %q", a)
			}
			isFloat = isFloat || strings.ContainsAny(a, ".eE")
		}
		first, step, last := 1.0, 1.0, 0.0
		switch len(nums) {
		case 1:
			last = nums[0]
		case 2:
			first, last = nums[0], nums[1]
		case 3:
			first, step, last = nums[0], nums[1], nums[2]
		default:
			return usagef("expected 1 to 3 numbers")
		}
		if step == 0 {
			return usagef("increment must not be zero")
		}
		sep := "\n"
		if s, ok := f.last('s'); ok {
			sep = s
		}
		const maxItems = 1_000_000
		w := bufio.NewWriter(inv.Stdout)
		n := 0
		for x := first; (step > 0 && x <= last+1e-9) || (step < 0 && x >= last-1e-9); x = first + float64(n)*step {
			if n > 0 {
				w.WriteString(sep)
			}
			if n >= maxItems {
				return fmt.Errorf("sequence longer than %d items", maxItems)
			}
			var err error
			if isFloat {
				_, err = w.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
			} else {
				_, err = w.WriteString(strconv.FormatInt(int64(math.Round(x)), 10))
			}
			if err != nil {
				return exitError(141) // reader went away (EPIPE), as with SIGPIPE
			}
			n++
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if n > 0 {
			w.WriteString("\n")
		}
		return w.Flush()
	})

// ---- sed (s/// only) ----

var sedCmd = command("sed", "substitute text with s/regex/replacement/flags",
	`usage: sed [-nE] [-e SCRIPT]... [SCRIPT] [file...]
Only substitution is supported: s/REGEX/REPLACEMENT/FLAGS, several separated by ';' or
newlines. Any delimiter may replace '/'. FLAGS: g (all), i (ignore case), p (print when
substituted; useful with -n), N (only the Nth match). REPLACEMENT may use & and \1-\9.
-E uses extended regex; without it, basic-regex escapes (\( \) \| \+ ...) work.
For line ranges, use head/tail; for deleting lines, use grep -v.`,
	runSed)

type sedSub struct {
	re     *regexp.Regexp
	repl   string
	global bool
	print  bool
	nth    int
}

func runSed(ctx context.Context, inv *engine.Invocation) error {
	f, err := parseFlags(inv.Args, "nEr", "e", map[string]byte{
		"quiet": 'n', "silent": 'n', "regexp-extended": 'E', "expression": 'e',
	}, true)
	if err != nil {
		return err
	}
	scripts := f.vals['e']
	operands := f.rest
	if len(scripts) == 0 {
		if len(operands) == 0 {
			return usagef("missing script")
		}
		scripts, operands = operands[:1], operands[1:]
	}
	var subs []sedSub
	for _, s := range scripts {
		parsed, err := parseSed(s, f.has('E') || f.has('r'))
		if err != nil {
			return err
		}
		subs = append(subs, parsed...)
	}
	return eachInput(inv, operands, func(in input) error {
		sc := newScanner(in.r)
		for sc.Scan() {
			line := sc.Text()
			for _, s := range subs {
				var did bool
				line, did = s.apply(line)
				if did && s.print {
					fmt.Fprintln(inv.Stdout, line)
				}
			}
			// Without -n the line is printed again, as sed does after a p flag.
			if !f.has('n') {
				fmt.Fprintln(inv.Stdout, line)
			}
		}
		return sc.Err()
	})
}

func parseSed(script string, extended bool) ([]sedSub, error) {
	var subs []sedSub
	rest := strings.TrimSpace(script)
	for rest != "" {
		if rest[0] == ';' || rest[0] == '\n' {
			rest = strings.TrimSpace(rest[1:])
			continue
		}
		if rest[0] != 's' || len(rest) < 2 {
			return nil, usagef("unsupported command %q: only s/// is supported (use grep, head or tail)", firstLine(rest))
		}
		delim := rest[1]
		parts, remaining, err := splitDelimited(rest[2:], delim, 2)
		if err != nil {
			return nil, err
		}
		flagEnd := strings.IndexAny(remaining, ";\n")
		if flagEnd < 0 {
			flagEnd = len(remaining)
		}
		sub := sedSub{repl: parts[1], nth: 1}
		pattern := parts[0]
		if !extended {
			pattern = breToERE(pattern)
		}
		icase := false
		for _, c := range strings.TrimSpace(remaining[:flagEnd]) {
			switch {
			case c == 'g':
				sub.global = true
			case c == 'p':
				sub.print = true
			case c == 'i' || c == 'I':
				icase = true
			case c >= '1' && c <= '9':
				sub.nth = int(c - '0')
			default:
				return nil, usagef("unsupported s flag %q", c)
			}
		}
		if icase {
			pattern = "(?i)" + pattern
		}
		if sub.re, err = regexp.Compile(pattern); err != nil {
			return nil, usagef("bad regex: %v", err)
		}
		subs = append(subs, sub)
		rest = strings.TrimSpace(remaining[flagEnd:])
	}
	return subs, nil
}

// splitDelimited reads n delim-terminated fields, honouring backslash-escaped
// delimiters, and returns the rest of the string.
func splitDelimited(s string, delim byte, n int) ([]string, string, error) {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			if s[i+1] == delim {
				cur.WriteByte(delim)
			} else {
				cur.WriteByte(c)
				cur.WriteByte(s[i+1])
			}
			i++
			continue
		}
		if c == delim {
			parts = append(parts, cur.String())
			cur.Reset()
			if len(parts) == n {
				return parts, s[i+1:], nil
			}
			continue
		}
		cur.WriteByte(c)
	}
	return nil, "", usagef("unterminated s command")
}

func (s sedSub) apply(line string) (string, bool) {
	matches := s.re.FindAllStringSubmatchIndex(line, -1)
	if len(matches) == 0 {
		return line, false
	}
	var b strings.Builder
	last, did := 0, false
	for i, m := range matches {
		if !s.global && i+1 != s.nth {
			continue
		}
		b.WriteString(line[last:m[0]])
		b.WriteString(expandRepl(s.repl, line, m))
		last, did = m[1], true
		if !s.global {
			break
		}
	}
	b.WriteString(line[last:])
	return b.String(), did
}

func expandRepl(repl, line string, m []int) string {
	group := func(n int) string {
		if 2*n+1 >= len(m) || m[2*n] < 0 {
			return ""
		}
		return line[m[2*n]:m[2*n+1]]
	}
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		switch {
		case c == '&':
			b.WriteString(group(0))
		case c == '\\' && i+1 < len(repl):
			i++
			switch n := repl[i]; {
			case n >= '0' && n <= '9':
				b.WriteString(group(int(n - '0')))
			case n == 'n':
				b.WriteByte('\n')
			case n == 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(n)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
