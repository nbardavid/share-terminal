// Package proto : framing typé du tunnel applicatif.
//
// Le canal chiffré (cf. internal/crypto) est un stream de bytes. Au-dessus,
// on encode des frames typées :
//
//	[1 byte type][4 bytes length BE][payload...]
//
// Types : FrameData (PTY → client), FrameInput (client → PTY, mode --write),
// FrameResize (cols, rows), FrameMeta (metadata host → client), FrameClose
// (close signalé).
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type FrameType uint8

const (
	FrameData   FrameType = 0x01 // PTY output (host → client)
	FrameInput  FrameType = 0x02 // user keystrokes (client → host, si --write)
	FrameResize FrameType = 0x03 // {cols uint16, rows uint16}
	FrameMeta   FrameType = 0x04 // metadata host → client (JSON court)
	FrameClose  FrameType = 0x05 // sentinelle de fin
)

// MaxPayload limite la taille d'un payload pour éviter qu'une frame foireuse
// alloue des gigaoctets. PTY chunks dépasse rarement quelques Ko.
const MaxPayload = 1 << 20 // 1 MiB

// ResizePayload est le format wire d'une frame resize.
type ResizePayload struct {
	Cols, Rows uint16
}

func (r ResizePayload) Bytes() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], r.Cols)
	binary.BigEndian.PutUint16(b[2:4], r.Rows)
	return b
}

func ParseResize(b []byte) (ResizePayload, error) {
	if len(b) != 4 {
		return ResizePayload{}, fmt.Errorf("resize payload must be 4 bytes, got %d", len(b))
	}
	return ResizePayload{
		Cols: binary.BigEndian.Uint16(b[0:2]),
		Rows: binary.BigEndian.Uint16(b[2:4]),
	}, nil
}

// Write encode et écrit une frame complète sur w.
func Write(w io.Writer, t FrameType, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("payload too large: %d > %d", len(payload), MaxPayload)
	}
	hdr := make([]byte, 5)
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// Read lit la prochaine frame depuis r.
func Read(r io.Reader) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	t := FrameType(hdr[0])
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("payload length out of range: %d", n)
	}
	if n == 0 {
		return t, nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return t, buf, nil
}

// ErrUnexpectedFrame est utile pour signaler qu'un type de frame n'était pas attendu dans ce contexte.
var ErrUnexpectedFrame = errors.New("unexpected frame type")
