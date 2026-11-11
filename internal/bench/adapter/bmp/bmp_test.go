package bmp

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/png"
	"testing"
)

// build makes a BMP. pixels[y][x] is RGB, y=0 is the top row.
func build(t *testing.T, bpp int, bitfields, topDown bool, pixels [][][3]uint8) []byte {
	t.Helper()
	h, w := len(pixels), len(pixels[0])
	stride := ((w*bpp + 31) / 32) * 4
	header := 14 + 40
	if bitfields {
		header += 12
	}
	b := make([]byte, header+stride*h)
	le := binary.LittleEndian
	copy(b, "BM")
	le.PutUint32(b[2:], uint32(len(b)))
	le.PutUint32(b[10:], uint32(header))
	le.PutUint32(b[14:], 40)
	le.PutUint32(b[18:], uint32(w))
	hh := int32(h)
	if topDown {
		hh = -hh
	}
	le.PutUint32(b[22:], uint32(hh))
	le.PutUint16(b[26:], 1)
	le.PutUint16(b[28:], uint16(bpp))
	if bitfields {
		le.PutUint32(b[30:], biBitfields)
		le.PutUint32(b[54:], 0xF800) // RGB565
		le.PutUint32(b[58:], 0x07E0)
		le.PutUint32(b[62:], 0x001F)
	}
	for y := 0; y < h; y++ {
		row := y
		if !topDown {
			row = h - 1 - y
		}
		p := b[header+row*stride:]
		for x := 0; x < w; x++ {
			c := pixels[y][x]
			switch bpp {
			case 24:
				p[x*3], p[x*3+1], p[x*3+2] = c[2], c[1], c[0]
			case 16:
				v := uint16(c[0]>>3)<<11 | uint16(c[1]>>2)<<5 | uint16(c[2]>>3)
				le.PutUint16(p[x*2:], v)
			}
		}
	}
	return b
}

var px = [][][3]uint8{
	{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}},
	{{255, 255, 255}, {0, 0, 0}, {255, 255, 0}},
}

func check(t *testing.T, b []byte) {
	t.Helper()
	img, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for y := range px {
		for x := range px[y] {
			got := img.At(x, y).(color.RGBA)
			want := px[y][x]
			if got.R != want[0] || got.G != want[1] || got.B != want[2] || got.A != 255 {
				t.Fatalf("(%d,%d) = %v, want %v", x, y, got, want)
			}
		}
	}
}

func TestDecode24BottomUp(t *testing.T)         { check(t, build(t, 24, false, false, px)) } // SDG/SDM
func TestDecode16BitfieldsTopDown(t *testing.T) { check(t, build(t, 16, true, true, px)) }   // SDS

func TestToPNGAndInfo(t *testing.T) {
	b := build(t, 16, true, true, px)
	inf, err := ParseInfo(b)
	if err != nil || inf.Width != 3 || inf.Height != 2 || !inf.TopDown || inf.BitsPerPixel != 16 || inf.FileSize != len(b) {
		t.Fatalf("%+v %v", inf, err)
	}
	p, err := ToPNG(b)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(p))
	if err != nil || img.Bounds().Dx() != 3 {
		t.Fatalf("png: %v", err)
	}
}

func TestDecodeErrors(t *testing.T) {
	good := build(t, 24, false, false, px)
	if _, err := Decode([]byte("not a bmp")); err == nil {
		t.Error("garbage")
	}
	if _, err := Decode(good[:len(good)-4]); err == nil {
		t.Error("truncated pixels")
	}
	bad := append([]byte{}, good...)
	binary.LittleEndian.PutUint16(bad[28:], 8) // palette formats are not supported
	if _, err := Decode(bad); err == nil {
		t.Error("8 bpp")
	}
	huge := append([]byte{}, good...)
	binary.LittleEndian.PutUint32(huge[18:], 1<<20)
	if _, err := ParseInfo(huge); err == nil {
		t.Error("implausible size")
	}
}
