package domain

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Quantity is a parsed engineering value such as "200m", "4Vpp" or "1.414Vrms".
type Quantity struct {
	Value float64
	Unit  string // canonical unit suffix as written ("V", "Vpp", "Vrms", "Hz", "s", ...); "" if none
}

var siPrefix = map[string]float64{
	"p": 1e-12, "n": 1e-9, "u": 1e-6, "µ": 1e-6, "m": 1e-3,
	"k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9,
}

// knownUnits maps accepted unit spellings to a canonical form.
var knownUnits = map[string]string{
	"": "", "v": "V", "vpp": "Vpp", "vrms": "Vrms", "hz": "Hz", "s": "s", "sec": "s",
	"a": "A", "ohm": "Ohm", "Ω": "Ohm", "f": "F", "dbm": "dBm", "db": "dB", "%": "%",
	"deg": "deg", "sa/s": "Sa/s", "pts": "pts",
}

// ParseQuantity parses a number with an optional SI prefix and unit:
// "1k", "200m", "4Vpp", "1.414Vrms", "-2.5V", "10MHz", "1e3", "50ohm".
// Note the case-sensitive prefixes: "m" is milli and "M" is mega.
func ParseQuantity(s string) (Quantity, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Quantity{}, fmt.Errorf("empty value")
	}
	n := numericPrefixLen(s)
	if n == 0 {
		return Quantity{}, fmt.Errorf("invalid number %q", s)
	}
	v, err := strconv.ParseFloat(s[:n], 64)
	if err != nil {
		return Quantity{}, fmt.Errorf("invalid number %q: %w", s, err)
	}
	rest := strings.TrimSpace(s[n:])
	if u, ok := knownUnits[strings.ToLower(rest)]; ok && !isBarePrefix(rest) {
		return Quantity{Value: v, Unit: u}, nil
	}
	for p, mult := range siPrefix {
		if !strings.HasPrefix(rest, p) {
			continue
		}
		if u, ok := knownUnits[strings.ToLower(rest[len(p):])]; ok {
			return Quantity{Value: v * mult, Unit: u}, nil
		}
	}
	return Quantity{}, fmt.Errorf("unknown unit %q in %q", rest, s)
}

// isBarePrefix reports whether rest is a lone SI prefix such as "m" or "M"
// that must not be mistaken for a unit.
func isBarePrefix(rest string) bool {
	_, ok := siPrefix[rest]
	return ok
}

// numericPrefixLen returns the length of the longest prefix of s that is a valid float.
func numericPrefixLen(s string) int {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return 0
	}
	// Exponent only if followed by digits, so "1e3" parses but "1em" does not eat the 'e'.
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		k := j
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k > j {
			i = k
		}
	}
	return i
}

// ParseIn parses s and checks that its unit (if given) is one of allowed.
// An empty unit is always accepted.
func ParseIn(s string, allowed ...string) (float64, string, error) {
	q, err := ParseQuantity(s)
	if err != nil {
		return 0, "", err
	}
	if q.Unit == "" {
		return q.Value, "", nil
	}
	for _, a := range allowed {
		if q.Unit == a {
			return q.Value, q.Unit, nil
		}
	}
	return 0, "", fmt.Errorf("unit %q not allowed here (want %s)", q.Unit, strings.Join(allowed, ", "))
}

// ParseInstrumentNumber parses a value as returned by an instrument, e.g.
// "2.00E+00", "-4.72E+00V", "1000HZ", "0.001S", "+4.99784414E+00".
// It returns ok=false for invalid-measurement markers such as "****".
func ParseInstrumentNumber(s string) (v float64, ok bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.Trim(s, "*") == "" {
		return 0, false, nil
	}
	n := numericPrefixLen(s)
	if n == 0 {
		return 0, false, fmt.Errorf("not a number: %q", s)
	}
	v, err = strconv.ParseFloat(s[:n], 64)
	if err != nil {
		return 0, false, err
	}
	mult, err := instrumentSuffix(strings.TrimSpace(s[n:]))
	if err != nil {
		return 0, false, fmt.Errorf("%q: %w", s, err)
	}
	return v * mult, true, nil
}

// instrumentSuffix returns the multiplier for what an instrument appends to a
// number: nothing, a bare SI prefix ("700K", "14M"), a unit ("V", "HZ",
// "Sa/s") or a prefixed unit ("700mV", "1.00GSa/s"). Anything else is an
// error, so a misread suffix never yields a number off by a factor of 1000.
func instrumentSuffix(rest string) (float64, error) {
	if rest == "" {
		return 1, nil
	}
	if _, ok := knownUnits[strings.ToLower(rest)]; ok {
		return 1, nil
	}
	if mult, ok := siPrefix[rest]; ok {
		return mult, nil
	}
	for p, mult := range siPrefix {
		if u, ok := strings.CutPrefix(rest, p); ok && u != "" {
			if _, ok := knownUnits[strings.ToLower(u)]; ok {
				return mult, nil
			}
		}
	}
	return 0, fmt.Errorf("unrecognized unit suffix %q", rest)
}

// Tolerance is an absolute or relative tolerance such as "0.05" or "3%".
type Tolerance struct {
	Value    float64
	Relative bool
}

// ParseTolerance parses "3%" (relative) or "0.05" / "50m" (absolute).
func ParseTolerance(s string) (Tolerance, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) || v < 0 {
			return Tolerance{}, fmt.Errorf("invalid tolerance %q", s)
		}
		return Tolerance{Value: v / 100, Relative: true}, nil
	}
	q, err := ParseQuantity(s)
	if err != nil {
		return Tolerance{}, fmt.Errorf("invalid tolerance %q: %w", s, err)
	}
	if math.IsInf(q.Value, 0) || math.IsNaN(q.Value) {
		return Tolerance{}, fmt.Errorf("invalid tolerance %q", s)
	}
	return Tolerance{Value: math.Abs(q.Value)}, nil
}

// Within reports whether got is within the tolerance of want.
func (t Tolerance) Within(want, got float64) bool {
	limit := t.Value
	if t.Relative {
		limit = math.Abs(want) * t.Value
	}
	return math.Abs(got-want) <= limit+1e-15
}

func (t Tolerance) String() string {
	if t.Relative {
		return strconv.FormatFloat(t.Value*100, 'g', -1, 64) + "%"
	}
	return strconv.FormatFloat(t.Value, 'g', -1, 64)
}

// Close reports whether a and b agree to within rel (relative) or abs (absolute),
// whichever is larger. Used to detect instrument clamping after a set.
func Close(a, b, rel, abs float64) bool {
	return math.Abs(a-b) <= math.Max(abs, rel*math.Max(math.Abs(a), math.Abs(b)))
}
