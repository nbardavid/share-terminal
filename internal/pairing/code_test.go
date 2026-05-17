package pairing

import (
	"strings"
	"testing"
)

func TestGenerateRoundTrip(t *testing.T) {
	for i := 0; i < 100; i++ {
		code := Generate()
		parts := strings.Split(code, "-")
		if len(parts) != Words {
			t.Fatalf("Generate() = %q, expected %d parts", code, Words)
		}
		if err := Validate(code); err != nil {
			t.Fatalf("Validate(%q) failed: %v", code, err)
		}
	}
}

func TestNormalize(t *testing.T) {
	// Choisit trois mots qu'on sait être dans la liste.
	code := wordlist[0] + "-" + wordlist[1] + "-" + wordlist[2]

	cases := []string{
		code,
		strings.ToUpper(code),
		strings.ReplaceAll(code, "-", " "),
		strings.ReplaceAll(code, "-", "_"),
		"  " + code + "\n",
	}
	for _, c := range cases {
		got, err := Normalize(c)
		if err != nil {
			t.Errorf("Normalize(%q) error: %v", c, err)
			continue
		}
		if got != code {
			t.Errorf("Normalize(%q) = %q, want %q", c, got, code)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	bad := []string{
		"",
		"only-two",
		"one-two-three-four",
		"meteor-cobalt-pasunmotdelaliste",
	}
	for _, c := range bad {
		if err := Validate(c); err == nil {
			t.Errorf("Validate(%q) should have failed", c)
		}
	}
}
