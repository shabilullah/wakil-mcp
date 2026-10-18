package aggregate

import "strings"

// matchPattern reports whether name matches a glob pattern. The pattern may
// contain '*' for any run of characters (including none) and '?' for exactly
// one character. Matching is case-insensitive, because operators writing
// "GitHub" in a filter should not silently match nothing.
//
// This is deliberately not path.Match: tool names are not paths, and
// path.Match's separator handling would be a surprise here.
func matchPattern(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	// Fast path: no wildcards means an exact comparison.
	if !strings.ContainsAny(pattern, "*?") {
		return strings.EqualFold(pattern, name)
	}
	return globFold([]rune(strings.ToLower(pattern)), []rune(strings.ToLower(name)))
}

// globFold matches a lowercased pattern against a lowercased name. It walks
// the pattern with backtracking on '*' so repeated wildcards stay linear in
// practice.
func globFold(pattern, name []rune) bool {
	p, n := 0, 0
	starP, starN := -1, 0

	for n < len(name) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case p < len(pattern) && pattern[p] == '*':
			// Remember the position and try matching zero characters first.
			starP = p
			starN = n
			p++
		case starP >= 0:
			// Backtrack: let the last '*' absorb one more character.
			starN++
			n = starN
			p = starP + 1
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// matchAny reports whether name matches any pattern in the list.
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if matchPattern(p, name) {
			return true
		}
	}
	return false
}
