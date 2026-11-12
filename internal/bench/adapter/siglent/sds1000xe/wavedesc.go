package sds1000xe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// WaveDesc is the subset of the 346-byte LeCroy-style WAVEDESC block that
// SDS1000X-E scopes return for "Cn:WF? DESC". All fields are little-endian.
type WaveDesc struct {
	DescriptorName  string  `json:"descriptor_name"`
	TemplateName    string  `json:"template_name"`
	CommType        int16   `json:"comm_type"` // 0 = byte, 1 = word
	WaveDescLen     int32   `json:"wave_descriptor_len"`
	WaveArray1      int32   `json:"wave_array_1"`
	InstrumentName  string  `json:"instrument_name"`
	WaveArrayCount  int32   `json:"wave_array_count"`
	PointsPerScreen int32   `json:"points_per_screen"`
	FirstValidPoint int32   `json:"first_valid_point"`
	LastValidPoint  int32   `json:"last_valid_point"`
	SparsingFactor  int32   `json:"sparsing_factor"`
	VerticalGain    float32 `json:"vertical_gain"` // on SDS1000X-E this is volts/div
	VerticalOffset  float32 `json:"vertical_offset"`
	NominalBits     int16   `json:"nominal_bits"`
	HorizInterval   float32 `json:"horiz_interval"` // seconds per sample (1/SARA)
	HorizOffset     float64 `json:"horiz_offset"`
	TimebaseEnum    int16   `json:"timebase_enum"`
	VertCoupling    int16   `json:"vert_coupling"`
	ProbeAtt        float32 `json:"probe_att"`
	WaveSource      int16   `json:"wave_source"` // 0 = C1
}

// WaveDescSize is the descriptor length on SDS1000X-E.
const WaveDescSize = 346

// DecodeWaveDesc decodes a WAVEDESC block.
func DecodeWaveDesc(b []byte) (WaveDesc, error) {
	if len(b) < WaveDescSize {
		return WaveDesc{}, fmt.Errorf("WAVEDESC too short: %d bytes, want %d", len(b), WaveDescSize)
	}
	if !bytes.HasPrefix(b, []byte("WAVEDESC")) {
		return WaveDesc{}, fmt.Errorf("WAVEDESC: bad descriptor name %q", cstr(b[0:16]))
	}
	le := binary.LittleEndian
	i16 := func(off int) int16 { return int16(le.Uint16(b[off:])) }
	i32 := func(off int) int32 { return int32(le.Uint32(b[off:])) }
	f32 := func(off int) float32 { return math.Float32frombits(le.Uint32(b[off:])) }
	f64 := func(off int) float64 { return math.Float64frombits(le.Uint64(b[off:])) }
	if order := i16(34); order != 1 {
		return WaveDesc{}, fmt.Errorf("WAVEDESC: unsupported byte order %d (want 1 = little-endian)", order)
	}
	return WaveDesc{
		DescriptorName:  cstr(b[0:16]),
		TemplateName:    cstr(b[16:32]),
		CommType:        i16(32),
		WaveDescLen:     i32(36),
		WaveArray1:      i32(60),
		InstrumentName:  cstr(b[76:92]),
		WaveArrayCount:  i32(116),
		PointsPerScreen: i32(120),
		FirstValidPoint: i32(124),
		LastValidPoint:  i32(128),
		SparsingFactor:  i32(136),
		VerticalGain:    f32(156),
		VerticalOffset:  f32(160),
		NominalBits:     i16(172),
		HorizInterval:   f32(176),
		HorizOffset:     f64(180),
		TimebaseEnum:    i16(324),
		VertCoupling:    i16(326),
		ProbeAtt:        f32(328),
		WaveSource:      i16(344),
	}, nil
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// CodesPerDiv is the ADC code span of one vertical division on SDS1000X-E.
const CodesPerDiv = 25

// HorizontalDivs is the number of horizontal grid divisions.
const HorizontalDivs = 14

// CodeToVolts converts a raw int8 ADC code: V = code * vdiv/25 - offset.
func CodeToVolts(code int8, vdiv, offset float64) float64 {
	return float64(code)*vdiv/CodesPerDiv - offset
}
