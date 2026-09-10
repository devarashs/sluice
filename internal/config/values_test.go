package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	valid := map[string]time.Duration{
		"0s":    0,
		"250ms": 250 * time.Millisecond,
		"30s":   30 * time.Second,
		"1m30s": 90 * time.Second,
		"2h":    2 * time.Hour,
	}
	for text, want := range valid {
		got, err := ParseDuration(text)
		if err != nil {
			t.Errorf("%q: unexpected error %v", text, err)
			continue
		}
		if got.Duration() != want {
			t.Errorf("%q = %v, want %v", text, got, want)
		}
	}

	invalid := map[string]string{
		"":     "empty",
		"30":   "not recognised",
		"-5s":  "must not be negative",
		"soon": "not recognised",
		"5 s":  "not recognised",
	}
	for text, wantSubstring := range invalid {
		_, err := ParseDuration(text)
		if err == nil || !strings.Contains(err.Error(), wantSubstring) {
			t.Errorf("%q: err = %v, want it to contain %q", text, err, wantSubstring)
		}
	}
}

func TestDurationJSONRoundTrip(t *testing.T) {
	var holder struct {
		Timeout Duration `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(`{"timeout": "45s"}`), &holder); err != nil {
		t.Fatal(err)
	}
	if holder.Timeout.Duration() != 45*time.Second {
		t.Fatalf("timeout = %v", holder.Timeout)
	}
	out, err := json.Marshal(holder)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"timeout":"45s"}` {
		t.Fatalf("marshalled %s", out)
	}
}

func TestParseByteSize(t *testing.T) {
	valid := map[string]ByteSize{
		"0":        0,
		"4096":     4096,
		"4096B":    4096,
		"64K":      64 * KiB,
		"64KiB":    64 * KiB,
		"64 KiB":   64 * KiB,
		"64kib":    64 * KiB,
		"64KB":     64 * KB,
		"4M":       4 * MiB,
		"4MiB":     4 * MiB,
		"4MB":      4 * MB,
		"1G":       GiB,
		"1GiB":     GiB,
		"1GB":      GB,
		"1T":       TiB,
		"1TiB":     TiB,
		"1TB":      TB,
		"1.5GiB":   GiB + 512*MiB,
		"1.5KiB":   1536,
		"  2MiB  ": 2 * MiB,
	}
	for text, want := range valid {
		got, err := ParseByteSize(text)
		if err != nil {
			t.Errorf("%q: unexpected error %v", text, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %d, want %d", text, got, want)
		}
	}

	invalid := map[string]string{
		"":                       "empty",
		"KiB":                    "must start with a number",
		"64XB":                   "unknown unit",
		"1.5B":                   "not a whole number of bytes",
		"1.5.5KiB":               "is not a number",
		"9999999999999999999TiB": "too large",
		"-1":                     "must start with a number",
	}
	for text, wantSubstring := range invalid {
		_, err := ParseByteSize(text)
		if err == nil || !strings.Contains(err.Error(), wantSubstring) {
			t.Errorf("%q: err = %v, want it to contain %q", text, err, wantSubstring)
		}
	}
}

func TestByteSizeJSONAcceptsStringOrNumber(t *testing.T) {
	var holder struct {
		Size ByteSize `json:"size"`
	}
	for raw, want := range map[string]ByteSize{
		`{"size": "64KiB"}`: 64 * KiB,
		`{"size": 65536}`:   64 * KiB,
		`{"size": 1e3}`:     1000,
	} {
		holder.Size = 0
		if err := json.Unmarshal([]byte(raw), &holder); err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if holder.Size != want {
			t.Errorf("%s = %d, want %d", raw, holder.Size, want)
		}
	}
	for _, raw := range []string{`{"size": -1}`, `{"size": 1.5}`, `{"size": true}`, `{"size": "big"}`} {
		if err := json.Unmarshal([]byte(raw), &holder); err == nil {
			t.Errorf("%s: expected an error", raw)
		}
	}
}

func TestByteSizeString(t *testing.T) {
	for value, want := range map[ByteSize]string{
		0:           "0B",
		1500:        "1500B",
		64 * KiB:    "64KiB",
		4 * MiB:     "4MiB",
		GiB:         "1GiB",
		TiB:         "1TiB",
		GiB + 5*KiB: "1048581KiB",
		KiB + 1:     "1025B",
	} {
		if got := value.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int64(value), got, want)
		}
	}
}

func TestListenAddr(t *testing.T) {
	for _, value := range []string{":0", ":8080", "0.0.0.0:443", "127.0.0.1:9000", "[::1]:9000", "[::]:80", "localhost:8080", "eth0.example:1"} {
		if err := ListenAddr("listenAddr", value); err != nil {
			t.Errorf("%q: unexpected error %v", value, err)
		}
	}
	for value, wantSubstring := range map[string]string{
		"":              "is required",
		"8080":          "not in host:port form",
		"localhost":     "not in host:port form",
		":http":         `port "http" is not a number`,
		":65536":        "outside 0 to 65535",
		":-1":           "outside 0 to 65535",
		"::1:80":        "not in host:port form",
		"host:80:extra": "not in host:port form",
	} {
		err := ListenAddr("listenAddr", value)
		if err == nil || !strings.Contains(err.Error(), wantSubstring) {
			t.Errorf("%q: err = %v, want it to contain %q", value, err, wantSubstring)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "listenAddr: ") {
			t.Errorf("%q: err = %v, want the field name first", value, err)
		}
	}
}

func TestDialAddr(t *testing.T) {
	for _, value := range []string{"10.0.0.5:443", "example.org:80", "[2001:db8::1]:9000", "localhost:1"} {
		if err := DialAddr("targetAddr", value); err != nil {
			t.Errorf("%q: unexpected error %v", value, err)
		}
	}
	for value, wantSubstring := range map[string]string{
		"":           "is required",
		":443":       "needs a host",
		"host:0":     "outside 1 to 65535",
		"host:70000": "outside 1 to 65535",
		"host":       "not in host:port form",
	} {
		err := DialAddr("targetAddr", value)
		if err == nil || !strings.Contains(err.Error(), wantSubstring) {
			t.Errorf("%q: err = %v, want it to contain %q", value, err, wantSubstring)
		}
	}
}

func TestRequire(t *testing.T) {
	if err := Require("token", "secret"); err != nil {
		t.Errorf("unexpected error %v", err)
	}
	if err := Require("token", ""); err == nil || err.Error() != "token: is required" {
		t.Errorf("err = %v, want \"token: is required\"", err)
	}
}
