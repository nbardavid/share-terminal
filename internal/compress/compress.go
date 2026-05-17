// Package compress : wrap une net.Conn avec une compression deflate streaming.
//
// Intended layering :
//
//	app  →  compress.Conn  →  crypto.secureConn  →  raw net.Conn
//	         (flate)            (XChaCha20-Poly1305)
//
// La compression se fait AVANT le chiffrement (le seul ordre correct : du
// ciphertext est statistiquement indistinguable du random et ne compresse pas).
//
// Chaque Write() compresse les bytes via un flate.Writer et appelle Flush()
// derrière. Flush() émet un sync-block deflate (5 octets de marqueur) après
// chaque écriture, ce qui garantit que le peer peut décompresser sans
// attendre — pas de bufferisation arbitraire. Inconvénient : ~5 octets fixes
// par Write, donc pour des Writes minuscules (1-3 octets de frappe clavier)
// la compression coûte plus qu'elle ne rapporte. Le gain net reste largement
// positif parce que les gros bursts (PTY redraw, nvim/htop output) dominent
// la bande passante et compressent typiquement à 5-10x.
//
// Le contexte de compression est conservé entre Writes (pas de reset), donc
// le dictionnaire grandit et les ratios s'améliorent au fil de la session.
package compress

import (
	"compress/flate"
	"errors"
	"io"
	"net"
)

// Conn implémente net.Conn en compressant les Writes et décompressant les Reads.
type Conn struct {
	net.Conn
	zw *flate.Writer
	zr io.ReadCloser
}

// Wrap construit une Conn compressée par-dessus c. Les deux peers doivent
// appeler Wrap pour que la liaison fonctionne (sinon le décompresseur d'un
// côté reçoit du plaintext et explose).
func Wrap(c net.Conn) (*Conn, error) {
	zw, err := flate.NewWriter(c, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	zr := flate.NewReader(c)
	return &Conn{Conn: c, zw: zw, zr: zr}, nil
}

func (c *Conn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := c.zw.Write(p)
	if err != nil {
		return n, err
	}
	if err := c.zw.Flush(); err != nil {
		return n, err
	}
	return n, nil
}

func (c *Conn) Read(p []byte) (int, error) {
	return c.zr.Read(p)
}

// Close ferme proprement les deux compresseurs puis la conn sous-jacente.
func (c *Conn) Close() error {
	var errs []error
	if err := c.zw.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := c.zr.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := c.Conn.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
