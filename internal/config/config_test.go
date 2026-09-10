package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sample is a stand-in for a mode's configuration, exercising every shared
// value type and a nested object.
type sample struct {
	ListenAddr  string   `json:"listenAddr"`
	TargetAddr  string   `json:"targetAddr"`
	IdleTimeout Duration `json:"idleTimeout"`
	BufferSize  ByteSize `json:"bufferSize"`
	Limits      struct {
		MaxConnections int `json:"maxConnections"`
	} `json:"limits"`
}

func validateSample(cfg *sample) error {
	if err := ListenAddr("listenAddr", cfg.ListenAddr); err != nil {
		return err
	}
	if err := DialAddr("targetAddr", cfg.TargetAddr); err != nil {
		return err
	}
	if cfg.BufferSize == 0 {
		cfg.BufferSize = 32 * KiB
	}
	return nil
}

func TestDecodeHappyPathAppliesDefaultsFromValidate(t *testing.T) {
	raw := []byte(`{
		"listenAddr": ":8080",
		"targetAddr": "10.0.0.5:443",
		"idleTimeout": "1m30s",
		"limits": {"maxConnections": 1000}
	}`)
	cfg, err := Decode(raw, validateSample)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.IdleTimeout.Duration() != 90*time.Second {
		t.Errorf("idleTimeout = %v, want 1m30s", cfg.IdleTimeout)
	}
	if cfg.BufferSize != 32*KiB {
		t.Errorf("bufferSize = %v, want the 32KiB default from validate", cfg.BufferSize)
	}
	if cfg.Limits.MaxConnections != 1000 {
		t.Errorf("limits.maxConnections = %d, want 1000", cfg.Limits.MaxConnections)
	}
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	_, err := Decode([]byte(`{"listenAdr": ":8080"}`), (func(*sample) error)(nil))
	if err == nil || !strings.Contains(err.Error(), `unknown field "listenAdr"`) {
		t.Fatalf("err = %v, want the misspelled field named", err)
	}
}

func TestDecodeReportsSyntaxErrorLineAndColumn(t *testing.T) {
	raw := []byte("{\n  \"listenAddr\": \":8080\",\n  \"targetAddr\" \"x:1\"\n}")
	_, err := Decode(raw, (func(*sample) error)(nil))
	if err == nil || !strings.Contains(err.Error(), "syntax error at line 3, column") {
		t.Fatalf("err = %v, want a syntax error on line 3", err)
	}
}

func TestDecodeReportsTypeMismatchByFieldName(t *testing.T) {
	raw := []byte(`{"limits": {"maxConnections": "lots"}}`)
	_, err := Decode(raw, (func(*sample) error)(nil))
	if err == nil {
		t.Fatal("err = nil, want a type mismatch")
	}
	for _, want := range []string{"limits.maxConnections", "expected a number", "got string"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
}

func TestDecodeRejectsContentAfterObject(t *testing.T) {
	for name, raw := range map[string]string{
		"second object": `{"listenAddr": ":1"} {"listenAddr": ":2"}`,
		"stray brace":   `{"listenAddr": ":1"} }`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(raw), (func(*sample) error)(nil))
			if err == nil || !strings.Contains(err.Error(), "unexpected content after the configuration object") {
				t.Fatalf("err = %v, want trailing content rejected", err)
			}
		})
	}
}

func TestDecodeReturnsValidationErrorAsFieldError(t *testing.T) {
	_, err := Decode([]byte(`{"listenAddr": ":8080", "targetAddr": ":443"}`), validateSample)
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) {
		t.Fatalf("err = %v, want a *FieldError", err)
	}
	if fieldErr.Field != "targetAddr" {
		t.Errorf("field = %q, want targetAddr", fieldErr.Field)
	}
}

func TestDecodeRejectsBareNumberForDuration(t *testing.T) {
	_, err := Decode([]byte(`{"idleTimeout": 30}`), (func(*sample) error)(nil))
	if err == nil || !strings.Contains(err.Error(), "duration 30: must be a quoted string with a unit") {
		t.Fatalf("err = %v, want the unitless number rejected", err)
	}
}

func TestDecodeLeavesFieldsUnchangedOnNull(t *testing.T) {
	cfg, err := Decode([]byte(`{"idleTimeout": null, "bufferSize": null}`), (func(*sample) error)(nil))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.IdleTimeout != 0 || cfg.BufferSize != 0 {
		t.Fatalf("null should leave zero values, got %v and %v", cfg.IdleTimeout, cfg.BufferSize)
	}
}

func TestLoadReadsFileAndPrefixesErrorsWithPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forward.json")
	if err := os.WriteFile(path, []byte(`{"listenAddr": ":8080", "targetAddr": "example.org:80"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, validateSample)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TargetAddr != "example.org:80" {
		t.Errorf("targetAddr = %q", cfg.TargetAddr)
	}

	_, err = Load(filepath.Join(dir, "missing.json"), validateSample)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}

	if err := os.WriteFile(path, []byte(`{"nope": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path, validateSample)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want the path in the message", err)
	}
}

func TestLineAndColumn(t *testing.T) {
	raw := []byte("ab\ncd\nef")
	cases := []struct {
		offset       int64
		line, column int
	}{
		{0, 1, 1},
		{2, 1, 3},
		{3, 2, 1},
		{4, 2, 2},
		{7, 3, 2},
		{99, 3, 3}, // clamped to the end
		{-5, 1, 1}, // clamped to the start
	}
	for _, c := range cases {
		line, column := lineAndColumn(raw, c.offset)
		if line != c.line || column != c.column {
			t.Errorf("offset %d: got %d:%d, want %d:%d", c.offset, line, column, c.line, c.column)
		}
	}
}
