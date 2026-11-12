package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"strconv"

	"github.com/jescarri/benchctl/internal/bench/domain"
)

// WriteWaveformCSV writes "t,v" rows.
func WriteWaveformCSV(w io.Writer, wf domain.Waveform) error {
	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintf(bw, "# channel=C%d points=%d dt=%g t0=%g vdiv=%g offset=%g\nt,v\n",
		wf.Channel, wf.Points, wf.Dt, wf.T0, wf.VDiv, wf.Offset); err != nil {
		return err
	}
	line := make([]byte, 0, 48)
	for i, v := range wf.Volts {
		line = strconv.AppendFloat(line[:0], wf.Time(i), 'g', 9, 64)
		line = append(line, ',')
		line = strconv.AppendFloat(line, v, 'g', 6, 64)
		line = append(line, '\n')
		if _, err := bw.Write(line); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// Clipped reports whether the trace leaves the 8 vertical divisions of the
// screen (±4 div around the channel offset), where the ADC saturates and
// amplitude readings are no longer trustworthy.
func Clipped(wf domain.Waveform) bool {
	if wf.VDiv <= 0 {
		return false
	}
	limit := 4 * wf.VDiv * 0.98
	for _, v := range wf.Volts {
		if math.Abs(v+wf.Offset) >= limit {
			return true
		}
	}
	return false
}

// WriteWaveformJSON writes the waveform with metadata.
func WriteWaveformJSON(w io.Writer, wf domain.Waveform) error {
	return json.NewEncoder(w).Encode(wf)
}

// WaveSummary describes a capture without the samples (CLI stdout).
type WaveSummary struct {
	Channel    int      `json:"channel"`
	Points     int      `json:"points"`
	T0         float64  `json:"t0"`
	Dt         float64  `json:"dt"`
	Duration   float64  `json:"duration"`
	VDiv       float64  `json:"vdiv"`
	Offset     float64  `json:"offset"`
	Min        float64  `json:"vmin"`
	Max        float64  `json:"vmax"`
	Mean       float64  `json:"vmean"`
	PeakFreqHz float64  `json:"fft_peak_hz"`
	Clipped    bool     `json:"clipped"`
	Files      []string `json:"files,omitempty"`
}

// Summarize computes summary statistics and the FFT peak frequency.
func Summarize(wf domain.Waveform) WaveSummary {
	s := WaveSummary{Channel: wf.Channel, Points: wf.Points, T0: wf.T0, Dt: wf.Dt,
		Duration: wf.Dt * float64(wf.Points), VDiv: wf.VDiv, Offset: wf.Offset}
	if len(wf.Volts) == 0 {
		return s
	}
	s.Min, s.Max = math.Inf(1), math.Inf(-1)
	sum := 0.0
	for _, v := range wf.Volts {
		s.Min, s.Max = math.Min(s.Min, v), math.Max(s.Max, v)
		sum += v
	}
	s.Mean = sum / float64(len(wf.Volts))
	s.Clipped = Clipped(wf)
	s.PeakFreqHz = PeakFrequency(wf.Volts, wf.Dt)
	return s
}

// PeakFrequency returns the frequency of the largest non-DC spectral bin,
// refined by parabolic interpolation. Uses a radix-2 FFT on a Hann-windowed
// copy truncated to a power of two.
//
// A long record is never transformed whole (a 14 Mpts capture would allocate
// hundreds of MB). It is cut into consecutive full-rate windows of maxFFT
// points covering the whole record, and the strongest peak across them is
// the dominant frequency. When that peak lies within the band a box-decimated
// copy of the record can represent, the decimated copy is analysed instead
// for its finer frequency resolution; otherwise the decimated peak would be
// an alias, so the full-rate one is returned.
func PeakFrequency(v []float64, dt float64) float64 {
	const maxFFT = 1 << 20
	step := (len(v) + maxFFT - 1) / maxFFT
	if step <= 1 {
		f, _ := peakFFT(v, dt)
		return f
	}
	full, best := 0.0, -1.0
	for start := 0; start < len(v); start += maxFFT {
		if start+maxFFT > len(v) {
			start = len(v) - maxFFT // last window ends at the record end
		}
		if f, m := peakFFT(v[start:start+maxFFT], dt); m > best {
			full, best = f, m
		}
	}
	if decNyquist := 1 / (2 * dt * float64(step)); full > 0.8*decNyquist {
		return full
	}
	d := make([]float64, 0, len(v)/step+1)
	for i := 0; i+step <= len(v); i += step {
		sum := 0.0
		for _, x := range v[i : i+step] {
			sum += x
		}
		d = append(d, sum/float64(step))
	}
	f, _ := peakFFT(d, dt*float64(step))
	return f
}

// peakFFT returns the interpolated peak frequency of v and that bin's
// magnitude (0, 0 when there is no usable peak).
func peakFFT(v []float64, dt float64) (float64, float64) {
	n := 1
	for n*2 <= len(v) {
		n *= 2
	}
	if n < 8 || dt <= 0 {
		return 0, 0
	}
	mean := 0.0
	for _, x := range v[:n] {
		mean += x
	}
	mean /= float64(n)
	buf := make([]complex128, n)
	for i := 0; i < n; i++ {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
		buf[i] = complex((v[i]-mean)*w, 0)
	}
	fft(buf)
	best, bi := 0.0, 0
	mag := make([]float64, n/2)
	for i := 1; i < n/2; i++ {
		mag[i] = cmplx.Abs(buf[i])
		if mag[i] > best {
			best, bi = mag[i], i
		}
	}
	if bi == 0 {
		return 0, 0
	}
	delta := 0.0
	if bi > 1 && bi < n/2-1 {
		a, b, c := mag[bi-1], mag[bi], mag[bi+1]
		if d := a - 2*b + c; d != 0 {
			delta = 0.5 * (a - c) / d
		}
	}
	return (float64(bi) + delta) / (float64(n) * dt), best
}

// fft is an in-place iterative radix-2 Cooley-Tukey FFT; len(a) must be a power of two.
func fft(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		ang := -2 * math.Pi / float64(size)
		wl := complex(math.Cos(ang), math.Sin(ang))
		for i := 0; i < n; i += size {
			w := complex(1, 0)
			for k := 0; k < size/2; k++ {
				u, t := a[i+k], w*a[i+k+size/2]
				a[i+k], a[i+k+size/2] = u+t, u-t
				w *= wl
			}
		}
	}
}
