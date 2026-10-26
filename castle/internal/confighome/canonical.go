package confighome

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Canonicalize renders a payload through the generic-map form: decode
// into any (objects become maps, encoding/json marshals map keys
// sorted) then re-marshal. Both hand-written payloads and
// struct-rendered records normalize through this one form, which is
// what makes rendered-vs-live equality checkable byte-for-byte.
func Canonicalize(payload []byte) ([]byte, error) {
	var generic any
	if err := json.Unmarshal(payload, &generic); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	// Encode appends a newline; Marshal does not — strip it so both
	// producers of canonical bytes agree.
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// CanonicalChecksum is the sha256 hex of the canonical form: the
// round-trip checksum the import verifies (castle record -> rendered CM
// == live CM).
func CanonicalChecksum(payload []byte) (string, error) {
	canonical, err := Canonicalize(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// EqualPayload reports whether two payloads are equal after
// canonicalization, with their shared checksum.
func EqualPayload(a, b []byte) (bool, string, error) {
	ca, err := Canonicalize(a)
	if err != nil {
		return false, "", err
	}
	cb, err := Canonicalize(b)
	if err != nil {
		return false, "", err
	}
	sum := sha256.Sum256(ca)
	checksum := hex.EncodeToString(sum[:])
	return bytes.Equal(ca, cb), checksum, nil
}
