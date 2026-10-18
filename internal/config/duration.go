package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Convenience multipliers used when filling in default durations.
const (
	second = time.Second
	minute = time.Minute
)

// Duration is a time.Duration that marshals to and from a human-readable
// string such as "15m" or "1h30s". Bare numbers are accepted on input and are
// interpreted as seconds, which keeps hand-written configs forgiving.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Or returns the value as a time.Duration, or def when the value is zero.
func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = 0
		return nil
	}
	// Try a JSON number first: treat it as seconds.
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"15m\" or a number of seconds: %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	// A bare numeric string is seconds too.
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(time.Duration(f * float64(time.Second)))
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(b []byte) error {
	return d.UnmarshalJSON([]byte(strconv.Quote(string(b))))
}
