// Package bmp decodes the Windows bitmaps Siglent instruments return for
// SCDP screen dumps, so they can be saved as PNG. Supported: uncompressed
// 24/32-bit (BI_RGB) and 16/32-bit BI_BITFIELDS, bottom-up or top-down.
// (golang.org/x/image/bmp does not handle 16-bit bitfields, which the
// SDS1000X-E uses.)
package bmp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/bits"
)

const (
	biRGB       = 0
	biBitfields = 3
)

// Info describes a bitmap header.
type Info struct {
	Width, Height int
	BitsPerPixel  int
	TopDown       bool
	Compression   uint32
	FileSize      int
}

// ParseInfo reads the file and DIB headers.
func ParseInfo(b []byte) (Info, error) {
	if len(b) < 54 || b[0] != 'B' || b[1] != 'M' {
		return Info{}, fmt.Errorf("not a BMP")
	}
	le := binary.LittleEndian
	h := int32(le.Uint32(b[22:]))
	inf := Info{
		FileSize:     int(le.Uint32(b[2:])),
		Width:        int(int32(le.Uint32(b[18:]))),
		Height:       int(h),
		BitsPerPixel: int(le.Uint16(b[28:])),
		Compression:  le.Uint32(b[30:]),
	}
	if h < 0 {
		inf.TopDown, inf.Height = true, int(-h)
	}
	if inf.Width <= 0 || inf.Height <= 0 || inf.Width > 1<<14 || inf.Height > 1<<14 {
		return inf, fmt.Errorf("bmp: implausible size %dx%d", inf.Width, inf.Height)
	}
	return inf, nil
}

type channel struct {
	mask  uint32
	shift int
	max   uint32
}

func newChannel(mask uint32) channel {
	if mask == 0 {
		return channel{}
	}
	shift := bits.TrailingZeros32(mask)
	return channel{mask: mask, shift: shift, max: mask >> shift}
}

func (c channel) get(v uint32) uint8 {
	if c.mask == 0 {
		return 0
	}
	return uint8(((v & c.mask) >> c.shift) * 255 / c.max)
}

// Decode decodes a BMP into an RGBA image.
func Decode(b []byte) (image.Image, error) {
	inf, err := ParseInfo(b)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	offset := int(le.Uint32(b[10:]))
	bpp := inf.BitsPerPixel

	var r, g, bl channel
	switch {
	case inf.Compression == biRGB && (bpp == 24 || bpp == 32):
	case inf.Compression == biRGB && bpp == 16:
		r, g, bl = newChannel(0x7C00), newChannel(0x03E0), newChannel(0x001F) // 5-5-5
	case inf.Compression == biBitfields && (bpp == 16 || bpp == 32):
		const masks = 14 + 40 // right after BITMAPINFOHEADER; V4/V5 headers keep them here too
		if len(b) < masks+12 {
			return nil, fmt.Errorf("bmp: truncated bitfield masks")
		}
		r = newChannel(le.Uint32(b[masks:]))
		g = newChannel(le.Uint32(b[masks+4:]))
		bl = newChannel(le.Uint32(b[masks+8:]))
	default:
		return nil, fmt.Errorf("bmp: unsupported format %d bpp, compression %d", bpp, inf.Compression)
	}

	stride := ((inf.Width*bpp + 31) / 32) * 4
	if offset < 0 || offset+stride*inf.Height > len(b) {
		return nil, fmt.Errorf("bmp: pixel data truncated (%d bytes, need %d)", len(b), offset+stride*inf.Height)
	}
	img := image.NewRGBA(image.Rect(0, 0, inf.Width, inf.Height))
	for y := 0; y < inf.Height; y++ {
		row := y
		if !inf.TopDown {
			row = inf.Height - 1 - y
		}
		p := b[offset+row*stride:]
		for x := 0; x < inf.Width; x++ {
			var c color.RGBA
			switch {
			case bpp == 24 || (bpp == 32 && inf.Compression == biRGB):
				n := bpp / 8
				c = color.RGBA{R: p[x*n+2], G: p[x*n+1], B: p[x*n], A: 255}
			case bpp == 16:
				v := uint32(le.Uint16(p[x*2:]))
				c = color.RGBA{R: r.get(v), G: g.get(v), B: bl.get(v), A: 255}
			default: // 32-bit bitfields
				v := le.Uint32(p[x*4:])
				c = color.RGBA{R: r.get(v), G: g.get(v), B: bl.get(v), A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img, nil
}

// ToPNG converts a BMP to PNG.
func ToPNG(b []byte) ([]byte, error) {
	img, err := Decode(b)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
