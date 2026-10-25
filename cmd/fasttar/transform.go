package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// transform is one sed-like "s/RE/REPL/[flags]" expression given with
// --transform/--xform. GNU tar replaces the first match it finds; 'g' makes
// it global and a digit N starts at the Nth match.
type transform struct {
	re     *regexp.Regexp
	repl   string
	global bool
	from   int  // 1-based occurrence to start replacing at
	none   bool // 'R': GNU tar's range flag matches nothing without a range
}

// parseTransform accepts any delimiter, like sed: s|a/b|c|i. An empty
// expression is a no-op, as in GNU tar.
func parseTransform(expr string) (*transform, error) {
	if expr == "" {
		return nil, nil
	}
	if len(expr) < 2 || expr[0] != 's' {
		return nil, &usageError{msg: "Invalid transform expression"}
	}
	delim := expr[1]
	if delim == '\\' || delim == '\n' || delim == '\r' || delim == '\t' {
		return nil, &usageError{msg: "Invalid transform expression"}
	}

	// The pattern runs to the next unescaped delimiter, the replacement to the
	// one after that, and everything left over is the flag list. GNU tar
	// requires the replacement to be closed by a delimiter: "s/a/b" is invalid
	// while "s/a/b/" is not.
	var pattern, repl strings.Builder
	esc := false
	i := 2
	for ; i < len(expr); i++ {
		c := expr[i]
		if esc {
			pattern.WriteByte('\\')
			pattern.WriteByte(c)
			esc = false
			continue
		}
		if c == '\\' {
			esc = true
			continue
		}
		if c == delim {
			break
		}
		pattern.WriteByte(c)
	}
	if i >= len(expr) {
		return nil, &usageError{msg: "Invalid transform expression"}
	}
	i++
	esc = false
	for ; i < len(expr); i++ {
		c := expr[i]
		if esc {
			repl.WriteByte('\\')
			repl.WriteByte(c)
			esc = false
			continue
		}
		if c == '\\' {
			esc = true
			continue
		}
		if c == delim {
			break
		}
		repl.WriteByte(c)
	}
	if i >= len(expr) {
		return nil, &usageError{msg: "Invalid transform expression"}
	}

	t := &transform{repl: repl.String(), from: 1}
	flags := expr[i+1:]
	var syntax string
	for i := 0; i < len(flags); i++ {
		c := flags[i]
		if c >= '0' && c <= '9' {
			j := i
			for j < len(flags) && flags[j] >= '0' && flags[j] <= '9' {
				j++
			}
			n, err := strconv.Atoi(flags[i:j])
			if err != nil {
				return nil, &usageError{msg: "Invalid transform expression"}
			}
			if n > 0 {
				t.from = n
			}
			i = j - 1
			continue
		}
		switch c {
		case 'g':
			t.global = true
		case 'i':
			syntax += "(?i)"
		case 'h', 'H', 'r', 's', 'S', 'x':
			// GNU tar accepts these flags; on member names they behave like
			// the default single replacement.
		case 'R':
			// GNU tar's range flag replaces only inside a range, and a range
			// cannot be written here, so nothing is replaced.
			t.none = true
		default:
			return nil, &usageError{msg: fmt.Sprintf("Unknown flag in transform expression: %c", c)}
		}
	}

	goPat, err := breToRE2(pattern.String())
	if err != nil {
		return nil, &usageError{msg: fmt.Sprintf("Invalid transform expression: %v", err)}
	}
	re, err := regexp.Compile(syntax + goPat)
	if err != nil {
		return nil, &usageError{msg: fmt.Sprintf("Invalid transform expression: %v", err)}
	}
	t.re = re
	return t, nil
}

// breToRE2 rewrites a POSIX basic regular expression, the dialect GNU tar's
// --transform accepts, into Go's RE2 syntax. Groups and operators are written
// \(...\), \{...\}, \+ and \? in a BRE; unescaped ( ) + ? | { } are literal,
// and a backslash before an ordinary character quotes it.
func breToRE2(pat string) (string, error) {
	var b strings.Builder
	prev := false // the previous token can carry a '*' repetition
	inClass := false
	classFirst := false
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		if inClass {
			if c == '[' && i+1 < len(pat) && strings.IndexByte(":.=", pat[i+1]) >= 0 {
				// POSIX class, collating symbol or equivalence class: copy it
				// whole so its ']' does not end the bracket expression.
				delim := pat[i+1]
				j := i + 2
				for j+1 < len(pat) && !(pat[j] == delim && pat[j+1] == ']') {
					j++
				}
				if j+1 >= len(pat) {
					return "", fmt.Errorf("unterminated bracket expression")
				}
				b.WriteString(pat[i : j+2])
				i = j + 1
				classFirst = false
				continue
			}
			b.WriteByte(c)
			if classFirst {
				switch c {
				case '^':
					// The ']' right after '^' is still a literal member.
					continue
				case ']':
					classFirst = false
					continue
				}
				classFirst = false
				continue
			}
			if c == ']' {
				inClass = false
				prev = true
			}
			continue
		}
		switch c {
		case '\\':
			i++
			if i >= len(pat) {
				return "", fmt.Errorf("trailing backslash")
			}
			switch d := pat[i]; d {
			case '(', '|':
				b.WriteByte(d)
				prev = false
			case ')':
				b.WriteByte(')')
				prev = true
			case '{', '}':
				b.WriteByte(d)
				prev = d == '}'
			case '+', '?':
				b.WriteByte(d)
				prev = true
			case '1', '2', '3', '4', '5', '6', '7', '8', '9':
				return "", fmt.Errorf("back references are not supported")
			case 'w', 'W', 's', 'S', 'b', 'B':
				b.WriteByte('\\')
				b.WriteByte(d)
				prev = true
			default:
				b.WriteString(regexp.QuoteMeta(string(d)))
				prev = true
			}
		case '*':
			if prev {
				b.WriteByte('*')
			} else {
				b.WriteString(`\*`)
			}
			prev = true
		case '[':
			b.WriteByte('[')
			inClass = true
			classFirst = true
			prev = false
		case '.', '^', '$':
			b.WriteByte(c)
			prev = c == '.'
		case '(', ')', '+', '?', '|', '{', '}', ']':
			// Literal in a BRE.
			b.WriteString(`\` + string(c))
			prev = true
		default:
			b.WriteByte(c)
			prev = true
		}
	}
	if inClass {
		return "", fmt.Errorf("unterminated bracket expression")
	}
	return b.String(), nil
}

// expand substitutes \0..\9 (the sed spelling GNU tar uses) with the captured
// groups.
func expand(repl string, subs []string) string {
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		if c == '\\' && i+1 < len(repl) {
			d := repl[i+1]
			if d >= '0' && d <= '9' {
				n := int(d - '0')
				if n < len(subs) {
					b.WriteString(subs[n])
				}
				i++
				continue
			}
			b.WriteByte(c)
			b.WriteByte(d)
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// apply runs the expression over name.
func (t *transform) apply(name string) string {
	if t == nil || t.none {
		return name
	}
	matches := t.re.FindAllStringSubmatchIndex(name, -1)
	if len(matches) == 0 {
		return name
	}
	start := t.from - 1
	if start < 0 {
		start = 0
	}
	if start >= len(matches) {
		return name
	}

	last := len(matches) - 1
	if !t.global {
		last = start
	}

	var out strings.Builder
	prev := 0
	for i, m := range matches {
		if i < start {
			continue
		}
		out.WriteString(name[prev:m[0]])
		out.WriteString(expand(t.repl, submatches(name, m)))
		prev = m[1]
		if i == last {
			break
		}
	}
	out.WriteString(name[prev:])
	return out.String()
}

func submatches(name string, idx []int) []string {
	subs := make([]string, len(idx)/2)
	for i := 0; i < len(idx)/2; i++ {
		if idx[2*i] < 0 {
			subs[i] = ""
		} else {
			subs[i] = name[idx[2*i]:idx[2*i+1]]
		}
	}
	return subs
}

// transformList is the chain built from --transform options.
type transformList []*transform

func buildTransforms(exprs []string) (transformList, error) {
	var tl transformList
	for _, e := range exprs {
		t, err := parseTransform(e)
		if err != nil {
			return nil, err
		}
		if t != nil {
			tl = append(tl, t)
		}
	}
	return tl, nil
}

func (tl transformList) apply(name string) string {
	for _, t := range tl {
		name = t.apply(name)
	}
	return name
}

func (tl transformList) empty() bool { return len(tl) == 0 }

// stripComponents removes the first n leading components of a member name and
// reports whether anything is left.
func stripComponents(name string, n int) (string, bool) {
	if n <= 0 {
		return name, true
	}
	rest := name
	for i := 0; i < n; i++ {
		idx := strings.Index(rest, "/")
		if idx < 0 {
			return "", false
		}
		rest = rest[idx+1:]
	}
	return rest, true
}
