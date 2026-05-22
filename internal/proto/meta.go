package proto

import (
	"encoding/json"
	"fmt"
)

// Meta is the metadata sent by the host to the client right after the
// handshake: who is sharing, and in what mode. Encoded as JSON inside a
// FrameMeta — not perf-critical and much more readable than tight binary.
type Meta struct {
	User string `json:"user"`
	Host string `json:"host"`
	// Write reports whether the host accepts client keyboard input.
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
