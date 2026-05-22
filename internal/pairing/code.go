// Package pairing: generation and parsing of short 3-word codes.
//
// Format: three lowercase words separated by "-", e.g. "meteor-cobalt-jungle".
// Words come from a fixed list of 256 entries — see wordlist.go.
package pairing

import (
	"crypto/rand"
	"fmt"
	"strings"
)

const Words = 3

// Generate draws a new 3-word code from crypto/rand.
func Generate() string {
	parts := make([]string, Words)
	for i := range parts {
		parts[i] = wordlist[randIndex()]
	}
	return strings.Join(parts, "-")
}

func randIndex() uint8 {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail on a healthy OS; if it does we
		// panic because continuing would be less safe.
		panic(fmt.Sprintf("crypto/rand failure: %v", err))
	}
	return b[0]
}

// Validate checks that a string has the form word-word-word and that each
// word is in the list.
func Validate(code string) error {
	parts := strings.Split(code, "-")
	if len(parts) != Words {
		return fmt.Errorf("code must contain %d words separated by '-' (got %d)", Words, len(parts))
	}
	for i, p := range parts {
		if !inList(strings.ToLower(p)) {
			return fmt.Errorf("word %d unknown: %q", i+1, p)
		}
	}
	return nil
}

func inList(w string) bool {
	for _, v := range wordlist {
		if v == w {
			return true
		}
	}
	return false
}

// Normalize converts the code to canonical form (lowercase, '-' separator).
// Returns an error if the code is invalid.
func Normalize(code string) (string, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	// Accept spaces or underscores as separators (use case: dictated codes).
	code = strings.ReplaceAll(code, " ", "-")
	code = strings.ReplaceAll(code, "_", "-")
	if err := Validate(code); err != nil {
		return "", err
	}
	return code, nil
}
