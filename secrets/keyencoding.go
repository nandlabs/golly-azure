package secrets

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxSecretNameLength is the longest secret name Azure Key Vault accepts.
const MaxSecretNameLength = 127

// KeyEncoder maps golly secret keys onto Azure Key Vault secret names and
// back.
//
// Key Vault secret names must match ^[0-9a-zA-Z-]{1,127}$, while golly keys
// routinely contain '/', '_' and '.' (secrets.Namespace joins prefix and key
// with '/'). Encode must return a valid Key Vault name or an error wrapping
// ErrInvalidKey. Decode is used by List to turn vault names back into golly
// keys; it reports ok=false for names that were not produced by Encode, and
// those secrets are omitted from List.
type KeyEncoder interface {
	Encode(key string) (string, error)
	Decode(name string) (key string, ok bool)
}

// DefaultKeyEncoder is the KeyEncoder used when none is configured.
//
// The encoding is an escape scheme, reversible and prefix-free:
//
//   - ASCII letters and digits are kept as they are.
//   - '-' becomes "--".
//   - Every other byte (including each byte of a multi-byte UTF-8 rune)
//     becomes '-' followed by two upper-case hex digits, e.g. '/' -> "-2F",
//     '_' -> "-5F", '.' -> "-2E".
//
// Examples:
//
//	"db-password"            -> "db--password"
//	"tenants/acme/api_key"   -> "tenants-2Facme-2Fapi-5Fkey"
//	"app.config"             -> "app-2Econfig"
//
// The encoded name must be 1..127 characters; longer keys are rejected with
// ErrInvalidKey. Each escaped byte costs three characters, so keys that are
// mostly punctuation hit the limit sooner.
//
// Key Vault secret names are case-insensitive: "ApiKey" and "apikey" address
// the same secret. The encoding preserves case (so List round-trips the key
// exactly as written) but cannot make the vault case-sensitive; avoid keys
// that differ only by case.
var DefaultKeyEncoder KeyEncoder = escapeEncoder{}

// RawKeyEncoder passes keys through unchanged after validating them against
// the Key Vault naming rules. Use it to address secrets created outside
// golly (e.g. via the portal or az CLI) whose names contain dashes that must
// not be escaped. Keys containing characters other than [0-9a-zA-Z-] are
// rejected with ErrInvalidKey.
var RawKeyEncoder KeyEncoder = rawEncoder{}

const hexDigits = "0123456789ABCDEF"

type escapeEncoder struct{}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func (escapeEncoder) Encode(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("%w: key must not be empty", ErrInvalidKey)
	}
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case isAlnum(c):
			b.WriteByte(c)
		case c == '-':
			b.WriteString("--")
		default:
			b.WriteByte('-')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0F])
		}
	}
	name := b.String()
	if len(name) > MaxSecretNameLength {
		return "", fmt.Errorf("%w: key %q encodes to %d characters (max %d)",
			ErrInvalidKey, key, len(name), MaxSecretNameLength)
	}
	return name, nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func (e escapeEncoder) Decode(name string) (string, bool) {
	if name == "" || len(name) > MaxSecretNameLength {
		return "", false
	}
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAlnum(c) {
			out = append(out, c)
			continue
		}
		if c != '-' || i+1 >= len(name) {
			return "", false
		}
		if name[i+1] == '-' {
			out = append(out, '-')
			i++
			continue
		}
		if i+2 >= len(name) {
			return "", false
		}
		hi, ok1 := unhex(name[i+1])
		lo, ok2 := unhex(name[i+2])
		if !ok1 || !ok2 {
			return "", false
		}
		v := hi<<4 | lo
		// Encode never escapes alphanumerics or '-', so such an escape is
		// not canonical and the name was not produced by this encoder.
		if isAlnum(v) || v == '-' {
			return "", false
		}
		out = append(out, v)
		i += 2
	}
	if !utf8.Valid(out) {
		return "", false
	}
	return string(out), true
}

type rawEncoder struct{}

func validName(name string) bool {
	if name == "" || len(name) > MaxSecretNameLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isAlnum(name[i]) && name[i] != '-' {
			return false
		}
	}
	return true
}

func (rawEncoder) Encode(key string) (string, error) {
	if !validName(key) {
		return "", fmt.Errorf("%w: %q is not a valid Key Vault secret name (^[0-9a-zA-Z-]{1,127}$)", ErrInvalidKey, key)
	}
	return key, nil
}

func (rawEncoder) Decode(name string) (string, bool) {
	return name, validName(name)
}

// EncodeKey encodes key with DefaultKeyEncoder.
func EncodeKey(key string) (string, error) { return DefaultKeyEncoder.Encode(key) }

// DecodeKey decodes a Key Vault secret name with DefaultKeyEncoder.
func DecodeKey(name string) (string, bool) { return DefaultKeyEncoder.Decode(name) }
