package vxi11

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// xdrEnc is a minimal XDR (RFC 4506) encoder for the types VXI-11 uses.
type xdrEnc struct{ b []byte }

func (e *xdrEnc) u32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *xdrEnc) i32(v int32)  { e.u32(uint32(v)) }
func (e *xdrEnc) boolean(v bool) {
	if v {
		e.u32(1)
	} else {
		e.u32(0)
	}
}

func (e *xdrEnc) opaque(p []byte) {
	e.u32(uint32(len(p)))
	e.b = append(e.b, p...)
	if pad := (4 - len(p)%4) % 4; pad > 0 {
		e.b = append(e.b, make([]byte, pad)...)
	}
}

func (e *xdrEnc) str(s string) { e.opaque([]byte(s)) }

// xdrDec is a minimal XDR decoder.
type xdrDec struct {
	b   []byte
	err error
}

var errShort = errors.New("xdr: short buffer")

func (d *xdrDec) u32() uint32 {
	if d.err != nil {
		return 0
	}
	if len(d.b) < 4 {
		d.err = errShort
		return 0
	}
	v := binary.BigEndian.Uint32(d.b)
	d.b = d.b[4:]
	return v
}

func (d *xdrDec) i32() int32 { return int32(d.u32()) }

func (d *xdrDec) opaque() []byte {
	n := int(d.u32())
	if d.err != nil {
		return nil
	}
	padded := n + (4-n%4)%4
	if n < 0 || len(d.b) < padded {
		d.err = errShort
		return nil
	}
	out := make([]byte, n)
	copy(out, d.b[:n])
	d.b = d.b[padded:]
	return out
}

func (d *xdrDec) str() string { return string(d.opaque()) }

// ---- ONC-RPC record marking (RFC 5531 §11) ----

const lastFragment = 0x80000000

// maxRecord bounds a reassembled record to protect against a corrupt length.
const maxRecord = 256 << 20

func writeRecord(w io.Writer, payload []byte) error {
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, lastFragment|uint32(len(payload)))
	copy(buf[4:], payload)
	_, err := w.Write(buf)
	return err
}

func readRecord(r io.Reader) ([]byte, error) {
	var out []byte
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, err
		}
		h := binary.BigEndian.Uint32(hdr[:])
		n := int(h &^ lastFragment)
		if len(out)+n > maxRecord {
			return nil, fmt.Errorf("rpc: record too large (%d bytes)", len(out)+n)
		}
		start := len(out)
		out = append(out, make([]byte, n)...)
		if _, err := io.ReadFull(r, out[start:]); err != nil {
			return nil, err
		}
		if h&lastFragment != 0 {
			return out, nil
		}
	}
}
