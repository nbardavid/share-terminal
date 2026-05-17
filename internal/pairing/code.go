// Package pairing : génération et parsing de codes courts à 3 mots.
//
// Format : trois mots minuscules séparés par "-", ex: "meteor-cobalt-jungle".
// Les mots viennent d'une liste fixe de 256 entrées — voir wordlist.go.
package pairing

import (
	"crypto/rand"
	"fmt"
	"strings"
)

const Words = 3

// Generate tire un nouveau code de 3 mots aléatoires (crypto/rand).
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
		// crypto/rand sur OS sain ne devrait jamais échouer ; si ça arrive
		// on panique car continuer serait moins sûr.
		panic(fmt.Sprintf("crypto/rand failure: %v", err))
	}
	return b[0]
}

// Validate vérifie qu'une chaîne a la forme word-word-word avec des mots de la liste.
func Validate(code string) error {
	parts := strings.Split(code, "-")
	if len(parts) != Words {
		return fmt.Errorf("le code doit contenir %d mots séparés par '-' (reçu %d)", Words, len(parts))
	}
	for i, p := range parts {
		if !inList(strings.ToLower(p)) {
			return fmt.Errorf("mot %d inconnu : %q", i+1, p)
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

// Normalize met le code en forme canonique (lowercase, séparateur '-').
// Retourne une erreur si le code est invalide.
func Normalize(code string) (string, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	// Tolère espaces ou underscores comme séparateurs (cas d'usage : utilisateur dicte).
	code = strings.ReplaceAll(code, " ", "-")
	code = strings.ReplaceAll(code, "_", "-")
	if err := Validate(code); err != nil {
		return "", err
	}
	return code, nil
}

