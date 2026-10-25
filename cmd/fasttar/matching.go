package main

import (
	"os"
	"strings"
)

// matchOpts mirrors the four switches GNU tar exposes under "File name
// matching options".
type matchOpts struct {
	wildcards  bool
	matchSlash bool
	anchored   bool
	ignoreCase bool
}

// excludeOpts are the defaults GNU tar uses for exclusion patterns.
func (o *Options) excludeOpts() matchOpts {
	return matchOpts{
		wildcards:  !o.NoWildcards,
		matchSlash: o.WildcardsMatchSlash,
		anchored:   o.Anchored,
		ignoreCase: o.IgnoreCase,
	}
}

// memberOpts are the defaults GNU tar uses for member name arguments: they are
// taken literally unless --wildcards was given.
func (o *Options) memberOpts() matchOpts {
	return matchOpts{
		wildcards:  o.Wildcards,
		matchSlash: o.WildcardsMatchSlash,
		anchored:   true, // member arguments always match from the start
		ignoreCase: o.IgnoreCase,
	}
}

// wildmatch implements the fnmatch subset GNU tar relies on: '*', '?',
// "[...]" (with ranges and [!...] negation) and '\' escapes. When matchSlash
// is false the wildcards stop at a '/'.
func wildmatch(pattern, name string, m matchOpts) bool {
	if m.ignoreCase {
		pattern = strings.ToLower(pattern)
		name = strings.ToLower(name)
	}
	p := []rune(pattern)
	s := []rune(name)

	if m.anchored {
		return matchRunes(p, s, m.matchSlash)
	}
	// Non-anchored patterns may match at the beginning of the name or right
	// after any '/', which is what makes "--exclude=sub" catch tree/sub.
	for i := 0; ; i++ {
		if matchRunes(p, s[i:], m.matchSlash) {
			return true
		}
		if i >= len(s) {
			return false
		}
		if s[i] != '/' {
			// Only restart at component boundaries.
			for i < len(s) && s[i] != '/' {
				i++
			}
			if i >= len(s) {
				return false
			}
		}
	}
}

func matchRunes(p, s []rune, matchSlash bool) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if len(p) == 0 {
				if matchSlash {
					return true
				}
				// '*' still has to stop at '/'
				return !containsRune(s, '/')
			}
			for i := 0; i <= len(s); i++ {
				if i > 0 && !matchSlash && s[i-1] == '/' {
					break
				}
				if matchRunes(p, s[i:], matchSlash) {
					return true
				}
				if i < len(s) && s[i] == '/' && !matchSlash {
					break
				}
			}
			return false
		case '?':
			if len(s) == 0 || (!matchSlash && s[0] == '/') {
				return false
			}
			p, s = p[1:], s[1:]
		case '[':
			r, n, ok := matchClass(p, s, matchSlash)
			if !ok {
				return false
			}
			p, s = p[n:], r
		case '\\':
			if len(p) > 1 {
				p = p[1:]
			}
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

func containsRune(s []rune, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// matchClass consumes one "[...]" expression. It returns the remaining name
// and how many runes of the pattern were consumed.
func matchClass(p, s []rune, matchSlash bool) ([]rune, int, bool) {
	if len(s) == 0 || (!matchSlash && s[0] == '/') {
		return s, 0, false
	}
	i := 1
	negate := false
	if i < len(p) && (p[i] == '!' || p[i] == '^') {
		negate = true
		i++
	}
	matched := false
	first := true
	for i < len(p) && (p[i] != ']' || first) {
		first = false
		var lo rune
		if p[i] == '\\' && i+1 < len(p) {
			i++
			lo = p[i]
			i++
		} else {
			lo = p[i]
			i++
		}
		hi := lo
		if i+1 < len(p) && p[i] == '-' && p[i+1] != ']' {
			i++
			if p[i] == '\\' && i+1 < len(p) {
				i++
			}
			hi = p[i]
			i++
		}
		if s[0] >= lo && s[0] <= hi {
			matched = true
		}
	}
	if i >= len(p) {
		// Unterminated class: treat '[' literally.
		return s, 1, false
	}
	i++ // consume ']'
	if matched == negate {
		return s, 0, false
	}
	return s[1:], i, true
}

// selector holds the member name arguments of this run together with their
// -C context.
type selector struct {
	args []selArg
	opts matchOpts
}

type selArg struct {
	name string // member name pattern
	base string // -C directory for output
}

func (s *selector) empty() bool { return s == nil || len(s.args) == 0 }

// match reports which argument selects the member, or -1.
func (s *selector) match(member string) int {
	if s.empty() {
		return -1
	}
	for i, a := range s.args {
		if s.selects(a.name, member) {
			return i
		}
	}
	return -1
}

// selects answers "does argument arg cover member name?" Both literal and
// wildcard arguments also cover everything below them, so naming a directory
// extracts its whole subtree.
func (s *selector) selects(arg, member string) bool {
	if s.opts.wildcards && isPattern(arg) {
		// The pattern itself, or one of the member's ancestor directories.
		if wildmatch(arg, member, s.opts) {
			return true
		}
		for i := strings.LastIndex(member, "/"); i >= 0; i = strings.LastIndex(member[:i], "/") {
			if wildmatch(arg, member[:i], s.opts) {
				return true
			}
		}
		return false
	}
	if arg == member {
		return true
	}
	return strings.HasPrefix(member, strings.TrimSuffix(arg, "/")+"/")
}

// isPattern reports whether an argument would be a glob under wildcards.
func isPattern(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// exclSet is the built exclusion list for one run.
type exclSet struct {
	opts matchOpts
	pats []string
}

func (e *exclSet) add(pat string) { e.pats = append(e.pats, pat) }

func (e *exclSet) empty() bool { return len(e.pats) == 0 }

// match reports whether name is excluded. GNU tar tries an exclusion pattern
// against the member name and -- unless --anchored was given -- against every
// component-boundary suffix of it. A pattern may also name a leading
// directory, which excludes everything below it (fnmatch's FNM_LEADING_DIR).
func (e *exclSet) match(name string) bool {
	for _, p := range e.pats {
		if e.matchPattern(p, name) {
			return true
		}
	}
	return false
}

func (e *exclSet) matchPattern(p, name string) bool {
	rest := name
	for {
		if e.exactMatch(p, rest) {
			return true
		}
		for i := 1; i < len(rest); i++ {
			if rest[i] == '/' && e.exactMatch(p, rest[:i]) {
				return true
			}
		}
		if e.opts.anchored {
			return false
		}
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
	}
}

// exactMatch compares one pattern with one candidate name. The suffix walk in
// matchPattern handles the non-anchored case, so matching here is always
// anchored at the start of the candidate.
func (e *exclSet) exactMatch(p, s string) bool {
	if e.opts.wildcards {
		m := e.opts
		m.anchored = true
		return wildmatch(p, s, m)
	}
	if e.opts.ignoreCase {
		return strings.EqualFold(p, s)
	}
	return p == s
}

// vcsDirs are the directories --exclude-vcs skips (GNU tar's own list).
var vcsDirs = []string{
	".git", ".gitignore", ".gitmodules", ".hg", ".hgignore", ".hgsub",
	".hgsubstate", ".hgtags", ".svn", ".bzr", ".bzrignore", "_darcs",
	"CVS", ".cvsignore", ".darcs",
}

// backupFiles are what --exclude-backups removes (emacs/vim/RCS noise).
var backupPats = []string{"*~", "#*#", ".#*", "%*%", "*,v", ".svn", "*.orig", "*.rej"}

// backupControl names accepted by --backup.
func backupMode(o *Options) string { return o.Backup }

// fileExists is a small helper used by the directory walkers.
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
