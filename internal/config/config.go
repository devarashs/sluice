// Package config loads and validates the JSON configuration files every mode
// reads, and defines the value types those files share.
//
// Decoding is strict on purpose. A misspelled key that is silently ignored is
// the most expensive kind of configuration mistake: the operator believes a
// limit or a timeout is in force when it is not. So unknown fields, type
// mismatches, bare numbers where a unit is needed, and content after the
// configuration object are all errors, and every error names what was wrong
// and where.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// FieldError reports that one configuration field is invalid. Field is the
// JSON path of the field as the operator wrote it, such as "limits.maxConnections".
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	return e.Field + ": " + e.Reason
}

// Invalid builds a FieldError for field with a formatted reason.
func Invalid(field, format string, args ...any) error {
	return &FieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Load reads the JSON file at path into a fresh T and validates it.
//
// validate runs after decoding and is where a mode applies its defaults and
// checks its invariants. It receives the decoded value and may mutate it. Pass
// nil when there is nothing to check.
func Load[T any](path string, validate func(*T) error) (*T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := Decode(raw, validate)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Decode is Load for configuration that is already in memory.
func Decode[T any](raw []byte, validate func(*T) error) (*T, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	cfg := new(T)
	if err := decoder.Decode(cfg); err != nil {
		return nil, describeDecodeError(raw, err)
	}

	// The first Decode stops at the end of the top-level value. Anything after
	// it, such as a second object or a stray brace, would otherwise be ignored,
	// and an operator who pasted two configs together would run the first
	// without knowing it.
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		offset := decoder.InputOffset()
		if syntaxErr := (*json.SyntaxError)(nil); errors.As(err, &syntaxErr) {
			offset = syntaxErr.Offset
		}
		line, column := lineAndColumn(raw, offset)
		return nil, fmt.Errorf("unexpected content after the configuration object at line %d, column %d", line, column)
	}

	if validate != nil {
		if err := validate(cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// describeDecodeError rewrites encoding/json errors into messages that say
// where the problem is in the operator's file rather than in Go's type system.
func describeDecodeError(raw []byte, err error) error {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		line, column := lineAndColumn(raw, syntaxErr.Offset)
		return fmt.Errorf("syntax error at line %d, column %d: %s", line, column, syntaxErr.Error())
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "top level"
		}
		line, column := lineAndColumn(raw, typeErr.Offset)
		return fmt.Errorf("field %s at line %d, column %d: expected %s, got %s",
			field, line, column, describeType(typeErr.Type.String()), typeErr.Value)
	}

	// encoding/json reports an unknown key as a plain error string. Strip its
	// "json: " prefix so the message reads as ours.
	const unknownFieldPrefix = "json: unknown field "
	if strings.HasPrefix(err.Error(), unknownFieldPrefix) {
		return fmt.Errorf("unknown field %s", strings.TrimPrefix(err.Error(), unknownFieldPrefix))
	}

	return err
}

// describeType turns a Go type name from an UnmarshalTypeError into the JSON
// kind the operator should have written.
func describeType(goType string) string {
	switch {
	case goType == "string":
		return "a string"
	case goType == "bool":
		return "true or false"
	case strings.HasPrefix(goType, "int"), strings.HasPrefix(goType, "uint"), strings.HasPrefix(goType, "float"):
		return "a number"
	case strings.HasPrefix(goType, "[]"):
		return "a list"
	case strings.HasPrefix(goType, "map["), strings.HasPrefix(goType, "*"):
		return "an object"
	default:
		return goType
	}
}

// lineAndColumn converts a byte offset into the 1-based line and column an
// editor would show. offset beyond the end of raw is clamped, which is what
// encoding/json reports for an unexpected end of input.
func lineAndColumn(raw []byte, offset int64) (line, column int) {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(raw)) {
		offset = int64(len(raw))
	}
	consumed := raw[:offset]
	line = 1 + bytes.Count(consumed, []byte{'\n'})
	lastNewline := bytes.LastIndexByte(consumed, '\n')
	column = int(offset) - lastNewline
	return line, column
}
