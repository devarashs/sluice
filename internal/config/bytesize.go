package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ByteSize is a byte count that decodes from a JSON string with a unit, such
// as "64KiB", "4MB", or "1.5GiB", or from a bare non-negative JSON number
// meaning bytes.
//
// Binary units (KiB, MiB, GiB, TiB) are powers of 1024 and decimal units
// (KB, MB, GB, TB) are powers of 1000, as the IEC and SI standards define
// them. A bare K, M, G, or T suffix is treated as binary, because "a 64K
// buffer" has meant 65536 bytes to every systems programmer for fifty years
// and refusing it would be pedantry that costs operators time.
type ByteSize int64

// Binary unit multipliers.
const (
	KiB ByteSize = 1 << (10 * (iota + 1))
	MiB
	GiB
	TiB
)

// Decimal unit multipliers.
const (
	KB ByteSize = 1000
	MB          = 1000 * KB
	GB          = 1000 * MB
	TB          = 1000 * GB
)

// byteUnits maps a lower-cased suffix to its multiplier.
var byteUnits = map[string]ByteSize{
	"":    1,
	"b":   1,
	"k":   KiB,
	"kib": KiB,
	"kb":  KB,
	"m":   MiB,
	"mib": MiB,
	"mb":  MB,
	"g":   GiB,
	"gib": GiB,
	"gb":  GB,
	"t":   TiB,
	"tib": TiB,
	"tb":  TB,
}

// Bytes returns the value as a plain integer.
func (s ByteSize) Bytes() int64 {
	return int64(s)
}

// String formats the value with the largest binary unit that divides it
// exactly, so 65536 reads back as "64KiB" but 1500 stays "1500B".
func (s ByteSize) String() string {
	switch {
	case s != 0 && s%TiB == 0:
		return strconv.FormatInt(int64(s/TiB), 10) + "TiB"
	case s != 0 && s%GiB == 0:
		return strconv.FormatInt(int64(s/GiB), 10) + "GiB"
	case s != 0 && s%MiB == 0:
		return strconv.FormatInt(int64(s/MiB), 10) + "MiB"
	case s != 0 && s%KiB == 0:
		return strconv.FormatInt(int64(s/KiB), 10) + "KiB"
	default:
		return strconv.FormatInt(int64(s), 10) + "B"
	}
}

// UnmarshalJSON implements json.Unmarshaler. A quoted string goes through
// ParseByteSize; a bare JSON number is a count of bytes. JSON null leaves the
// value unchanged, matching how encoding/json treats null for every other
// type.
func (s *ByteSize) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return fmt.Errorf("size %s: not a valid string", string(raw))
		}
		parsed, err := ParseByteSize(text)
		if err != nil {
			return err
		}
		*s = parsed
		return nil
	}

	// JSON numbers may carry a fraction or an exponent (1e6 is legal JSON), so
	// parse as a float and insist on a whole, non-negative byte count.
	value, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return fmt.Errorf("size %s: must be a number of bytes or a quoted string with a unit, such as \"64KiB\"", string(raw))
	}
	parsed, err := wholeBytes(value, string(raw))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// MarshalJSON implements json.Marshaler.
func (s ByteSize) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(s.String())), nil
}

// ParseByteSize parses text such as "64KiB", "4 MB", "1.5GiB", or "4096".
// The result must be a whole number of bytes, so "1.5KiB" is 1536 and "1.5B"
// is an error rather than a silent rounding.
func ParseByteSize(text string) (ByteSize, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, errors.New("size: empty; write a number with a unit, such as \"64KiB\", or omit the field")
	}

	digitsEnd := 0
	for digitsEnd < len(trimmed) && (trimmed[digitsEnd] >= '0' && trimmed[digitsEnd] <= '9' || trimmed[digitsEnd] == '.') {
		digitsEnd++
	}
	number, unit := trimmed[:digitsEnd], strings.ToLower(strings.TrimSpace(trimmed[digitsEnd:]))
	if number == "" {
		return 0, fmt.Errorf("size %q: must start with a number", text)
	}

	multiplier, known := byteUnits[unit]
	if !known {
		return 0, fmt.Errorf("size %q: unknown unit %q; use B, KiB, MiB, GiB, TiB, KB, MB, GB, or TB", text, unit)
	}

	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %q is not a number", text, number)
	}
	return wholeBytes(value*float64(multiplier), text)
}

// wholeBytes converts a computed byte count into a ByteSize, rejecting
// negative, fractional, and overflowing values. text is what the operator
// wrote, quoted in the error so they can find it.
func wholeBytes(total float64, text string) (ByteSize, error) {
	if total < 0 {
		return 0, fmt.Errorf("size %q: must not be negative", text)
	}
	// float64(math.MaxInt64) rounds up to 2^63, which int64 cannot hold, so
	// the comparison must exclude equality.
	if total >= math.MaxInt64 {
		return 0, fmt.Errorf("size %q: too large", text)
	}
	if total != math.Trunc(total) {
		return 0, fmt.Errorf("size %q: is not a whole number of bytes", text)
	}
	return ByteSize(total), nil
}
