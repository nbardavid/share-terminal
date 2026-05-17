package proto

import (
	"encoding/json"
	"fmt"
)

// Meta est la metadata échangée juste après le handshake. Le client envoie
// la sienne en premier, le host répond avec la sienne après acceptation.
// Encodée en JSON dans une FrameMeta — pas critique en perf, plus lisible
// que du binaire serré, et évolutif (les champs futurs ajoutent juste des
// clés ignorées par les vieilles versions, cf. Features).
type Meta struct {
	User string `json:"user"`
	Host string `json:"host"`
	// Write indique si le host accepte la saisie clavier du client.
	Write bool `json:"write"`
	// Features liste les capacités optionnelles supportées par ce peer.
	// L'autre côté active uniquement celles qu'il supporte aussi
	// (intersection). Valeurs connues :
	//   "deflate" : compression flate streaming inside la couche AEAD.
	// Les peers v0.1.0/v0.1.1 ignorent ce champ → pas de compression
	// avec un peer antérieur, mais la session marche.
	Features []string `json:"features,omitempty"`
}

// HasFeature retourne true si la feature est listée dans Features.
func (m Meta) HasFeature(name string) bool {
	for _, f := range m.Features {
		if f == name {
			return true
		}
	}
	return false
}

// Feature names — constantes pour éviter les typos.
const (
	FeatureDeflate = "deflate"
)

func (m Meta) Bytes() ([]byte, error) {
	return json.Marshal(m)
}

func ParseMeta(b []byte) (Meta, error) {
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("parse meta: %w", err)
	}
	return m, nil
}
