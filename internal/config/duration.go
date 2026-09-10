package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Duration is a time.Duration that decodes from a JSON string with a unit,
// such as "30s", "1m30s", or "250ms".
//
// Bare numbers are rejected. Their unit would be a guess, and the guess is how
// `"idleTimeout": 30` turns into a 30-nanosecond timeout in one codebase and
// a 30-second one in the next. Negative durations are rejected too; every
// field that uses this type treats zero as "disabled" and has no meaning for
// less than zero.
type Duration time.Duration

// Duration returns the value as the standard library type.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// String formats the value the way it would be written in a config file.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// UnmarshalJSON implements json.Unmarshaler. JSON null leaves the value
// unchanged, matching how encoding/json treats null for every other type.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return fmt.Errorf("duration %s: must be a quoted string with a unit, such as \"30s\"", string(raw))
	}
	parsed, err := ParseDuration(text)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalJSON implements json.Marshaler so a config can be written back out
// in the same form it is read.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(d.String())), nil
}

// ParseDuration parses text in the form accepted in config files.
func ParseDuration(text string) (Duration, error) {
	if text == "" {
		return 0, errors.New("duration: empty; write a number with a unit, such as \"30s\", or omit the field")
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("duration %q: not recognised; write a number with a unit, such as \"30s\", \"1m30s\", or \"250ms\"", text)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("duration %q: must not be negative", text)
	}
	return Duration(parsed), nil
}
