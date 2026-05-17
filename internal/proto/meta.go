package proto

import (
	"encoding/json"
	"fmt"
)

// Meta est la metadata envoyée par le host au client juste après le
// handshake : qui partage, et dans quel mode. Encodée en JSON dans une
// FrameMeta — pas critique en perf et bien plus lisible que du binaire serré.
type Meta struct {
	User string `json:"user"`
	Host string `json:"host"`
	// Write indique si le host accepte la saisie clavier du client.
	Write bool `json:"write"`
}

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
