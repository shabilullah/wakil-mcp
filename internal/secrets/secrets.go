// Package secrets resolves credential references in configuration values.
//
// Secrets are never stored in config.json. A value that needs one holds a
// reference instead:
//
//	${env:GH_TOKEN}            read from an environment variable
//	${file:/run/secrets/tok}   read from a file, trailing whitespace trimmed
//
// References may be embedded in a larger string, so
// "Bearer ${env:TOKEN}" resolves to "Bearer <value>".
//
// Resolved values are passed to the upstream process. They are never returned
// by the HTTP API: every read path runs through Redact first.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// missingPolicy controls what happens when a reference cannot be resolved.
type missingPolicy int

const (
	// policyError fails resolution, which is right when spawning an upstream.
	policyError missingPolicy = iota
	// policyPlaceholder leaves the reference intact, which is right when
	// echoing config back to a UI.
	policyPlaceholder
)

// Resolver resolves ${env:...} and ${file:...} references.
type Resolver struct {
	// LookupEnv is swappable for tests. Defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// ReadFile is swappable for tests. Defaults to os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// AllowFileRefs gates ${file:...}. Disabled means those references are
	// treated as unresolved, which matters when config is supplied by someone
	// who should not be able to read arbitrary paths.
	AllowFileRefs bool
}

// New returns a Resolver reading from the real environment and filesystem.
func New() *Resolver {
	return &Resolver{
		LookupEnv:     os.LookupEnv,
		ReadFile:      os.ReadFile,
		AllowFileRefs: true,
	}
}

// MissingReferenceError reports a reference that could not be resolved.
type MissingReferenceError struct {
	Kind string // "env" or "file"
	Name string
}

func (e *MissingReferenceError) Error() string {
	return fmt.Sprintf("unresolved secret reference: ${%s:%s}", e.Kind, e.Name)
}

// IsMissing reports whether err is an unresolved reference.
func IsMissing(err error) bool {
	var m *MissingReferenceError
	return errors.As(err, &m)
}

// Resolve expands every reference in s, failing on the first one it cannot
// satisfy.
func (r *Resolver) Resolve(s string) (string, error) {
	return r.expand(s, policyError)
}

// ResolveMap resolves every value in m, returning a new map.
func (r *Resolver) ResolveMap(m map[string]string) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		rv, err := r.Resolve(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = rv
	}
	return out, nil
}

// ResolveSlice resolves every element in s, returning a new slice.
func (r *Resolver) ResolveSlice(s []string) ([]string, error) {
	if len(s) == 0 {
		return nil, nil
	}
	out := make([]string, len(s))
	for i, v := range s {
		rv, err := r.Resolve(v)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = rv
	}
	return out, nil
}

// HasReference reports whether s contains at least one reference.
func HasReference(s string) bool {
	return strings.Contains(s, "${")
}

// expand walks s replacing references. Unterminated or unknown schemes are
// left alone when tolerant, and are errors otherwise.
func (r *Resolver) expand(s string, policy missingPolicy) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		start := strings.Index(s[i:], "${")
		if start < 0 {
			b.WriteString(s[i:])
			break
		}
		start += i
		b.WriteString(s[i:start])

		end := strings.IndexByte(s[start:], '}')
		if end < 0 {
			// No closing brace. Not a reference, emit the rest verbatim.
			b.WriteString(s[start:])
			break
		}
		end += start
		body := s[start+2 : end]

		kind, name, ok := splitRef(body)
		if !ok {
			// Unknown scheme, leave the literal text alone.
			b.WriteString(s[start : end+1])
			i = end + 1
			continue
		}

		val, err := r.lookup(kind, name)
		if err != nil {
			if policy == policyPlaceholder {
				b.WriteString(s[start : end+1])
				i = end + 1
				continue
			}
			return "", err
		}
		b.WriteString(val)
		i = end + 1
	}
	return b.String(), nil
}

// Redact replaces every reference in s with a fixed marker so that resolved
// values cannot leak through an API response. A resolved value that is already
// known is replaced with the marker too.
func Redact(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		start := strings.Index(s[i:], "${")
		if start < 0 {
			b.WriteString(s[i:])
			break
		}
		start += i
		b.WriteString(s[i:start])

		end := strings.IndexByte(s[start:], '}')
		if end < 0 {
			b.WriteString(s[start:])
			break
		}
		end += start
		body := s[start+2 : end]
		if kind, name, ok := splitRef(body); ok {
			b.WriteString("${" + kind + ":" + name + "}")
		} else {
			b.WriteString(s[start : end+1])
		}
		i = end + 1
	}
	return b.String()
}

// RedactMap redacts every value in m, returning a new map.
func RedactMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = Redact(v)
	}
	return out
}

// RedactSlice redacts every element in s, returning a new slice.
func RedactSlice(s []string) []string {
	if len(s) == 0 {
		return s
	}
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = Redact(v)
	}
	return out
}

// RedactSecrets replaces any occurrence of a resolved secret value inside s
// with a marker. Use this on upstream output and log lines, where a secret may
// have been echoed back by the server itself.
func RedactSecrets(s string, values []string) string {
	for _, v := range values {
		if len(v) >= 8 && strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
}

// ReferenceSecretValues returns every resolved value reachable from env and
// headers. Callers pass the result to RedactSecrets so that secrets echoed by
// an upstream are scrubbed from logs.
func (r *Resolver) ReferenceSecretValues(env, headers map[string]string) []string {
	var out []string
	collect := func(m map[string]string) {
		for _, v := range m {
			if !HasReference(v) {
				continue
			}
			if resolved, err := r.Resolve(v); err == nil && resolved != v {
				out = append(out, resolved)
			}
		}
	}
	collect(env)
	collect(headers)
	return out
}

func (r *Resolver) lookup(kind, name string) (string, error) {
	switch kind {
	case "env":
		if name == "" {
			return "", &MissingReferenceError{Kind: kind, Name: name}
		}
		v, ok := r.LookupEnv(name)
		if !ok {
			return "", &MissingReferenceError{Kind: kind, Name: name}
		}
		return v, nil
	case "file":
		if !r.AllowFileRefs {
			return "", &MissingReferenceError{Kind: kind, Name: name}
		}
		if name == "" {
			return "", &MissingReferenceError{Kind: kind, Name: name}
		}
		b, err := r.ReadFile(name)
		if err != nil {
			return "", &MissingReferenceError{Kind: kind, Name: name}
		}
		return strings.TrimRight(string(b), "\r\n \t"), nil
	default:
		return "", &MissingReferenceError{Kind: kind, Name: name}
	}
}

// splitRef parses "env:NAME" or "file:/path" into its parts.
func splitRef(body string) (kind, name string, ok bool) {
	idx := strings.IndexByte(body, ':')
	if idx <= 0 {
		return "", "", false
	}
	kind = strings.ToLower(strings.TrimSpace(body[:idx]))
	name = strings.TrimSpace(body[idx+1:])
	if name == "" {
		return "", "", false
	}
	if kind != "env" && kind != "file" {
		return "", "", false
	}
	return kind, name, true
}
