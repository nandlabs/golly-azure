package secrets

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

var kvName = regexp.MustCompile(`^[0-9a-zA-Z-]{1,127}$`)

func TestDefaultKeyEncoder_Examples(t *testing.T) {
	cases := map[string]string{
		"simple":               "simple",
		"MixedCase123":         "MixedCase123",
		"db-password":          "db--password",
		"tenants/acme/api_key": "tenants-2Facme-2Fapi-5Fkey",
		"app.config":           "app-2Econfig",
		"a b":                  "a-20b",
		"--":                   "----",
		"é":                    "-C3-A9",
		"/leading":             "-2Fleading",
	}
	for in, want := range cases {
		got, err := EncodeKey(in)
		if err != nil {
			t.Errorf("Encode(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Encode(%q) = %q, want %q", in, got, want)
		}
		if !kvName.MatchString(got) {
			t.Errorf("Encode(%q) = %q is not a valid Key Vault name", in, got)
		}
		back, ok := DecodeKey(got)
		if !ok || back != in {
			t.Errorf("Decode(%q) = %q,%v want %q", got, back, ok, in)
		}
	}
}

func TestDefaultKeyEncoder_RoundTripAllBytes(t *testing.T) {
	// Every ASCII character, individually and in combination.
	var all strings.Builder
	for c := 1; c < 128; c++ {
		k := "k" + string(rune(c)) + "-x"
		enc, err := EncodeKey(k)
		if err != nil {
			t.Fatalf("Encode(%q): %v", k, err)
		}
		if !kvName.MatchString(enc) {
			t.Fatalf("Encode(%q) = %q invalid", k, enc)
		}
		if dec, ok := DecodeKey(enc); !ok || dec != k {
			t.Fatalf("round trip %q -> %q -> %q,%v", k, enc, dec, ok)
		}
		if c%3 == 0 {
			all.WriteRune(rune(c))
		}
	}
	enc, err := EncodeKey(all.String())
	if err != nil {
		t.Fatal(err)
	}
	if dec, ok := DecodeKey(enc); !ok || dec != all.String() {
		t.Fatalf("combined round trip failed")
	}
}

func TestDefaultKeyEncoder_Injective(t *testing.T) {
	keys := []string{"a-b", "a/b", "a_b", "a.b", "a--b", "a-2Fb", "a2Fb", "a-", "-a", "a"}
	seen := map[string]string{}
	for _, k := range keys {
		enc, err := EncodeKey(k)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[enc]; dup {
			t.Errorf("%q and %q both encode to %q", prev, k, enc)
		}
		seen[enc] = k
	}
}

func TestDefaultKeyEncoder_Limits(t *testing.T) {
	if _, err := EncodeKey(""); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("empty: %v", err)
	}
	ok127 := strings.Repeat("a", 127)
	if _, err := EncodeKey(ok127); err != nil {
		t.Errorf("127 chars: %v", err)
	}
	if _, err := EncodeKey(ok127 + "a"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("128 chars: %v", err)
	}
	// 42 slashes = 126 chars, 43 = 129.
	if _, err := EncodeKey(strings.Repeat("/", 42)); err != nil {
		t.Errorf("42 slashes: %v", err)
	}
	if _, err := EncodeKey(strings.Repeat("/", 43)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("43 slashes: %v", err)
	}
}

func TestDefaultKeyEncoder_DecodeRejectsForeignNames(t *testing.T) {
	for _, name := range []string{
		"",
		"trailing-",
		"short-2",
		"bad-ZZ",
		"lower-2f",   // non-canonical lower-case hex
		"alnum-41",   // escaped 'A' is not canonical
		"dash-2D",    // escaped '-' is not canonical
		"invalid-FF", // not valid UTF-8
		"under_score",
		strings.Repeat("a", 128),
	} {
		if k, ok := DecodeKey(name); ok {
			t.Errorf("Decode(%q) = %q, want rejection", name, k)
		}
	}
}

func TestRawKeyEncoder(t *testing.T) {
	if n, err := RawKeyEncoder.Encode("my-secret-1"); err != nil || n != "my-secret-1" {
		t.Errorf("Encode = %q, %v", n, err)
	}
	for _, bad := range []string{"", "a/b", "a_b", strings.Repeat("x", 128)} {
		if _, err := RawKeyEncoder.Encode(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Encode(%q) err = %v", bad, err)
		}
		if _, ok := RawKeyEncoder.Decode(bad); ok {
			t.Errorf("Decode(%q) accepted", bad)
		}
	}
	if k, ok := RawKeyEncoder.Decode("ok-name"); !ok || k != "ok-name" {
		t.Errorf("Decode = %q,%v", k, ok)
	}
}
