package scpi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ESR bits (IEEE 488.2 §11.5.1).
const (
	ESROPC = 1 << 0
	ESRQYE = 1 << 2
	ESRDDE = 1 << 3
	ESREXE = 1 << 4
	ESRCME = 1 << 5
	ESRPON = 1 << 7

	esrErrorMask = ESRQYE | ESRDDE | ESREXE | ESRCME
)

// DescribeESR names the error bits set in esr.
func DescribeESR(esr byte) string {
	var parts []string
	if esr&ESRCME != 0 {
		parts = append(parts, "command error")
	}
	if esr&ESREXE != 0 {
		parts = append(parts, "execution error")
	}
	if esr&ESRDDE != 0 {
		parts = append(parts, "device-dependent error")
	}
	if esr&ESRQYE != 0 {
		parts = append(parts, "query error")
	}
	return strings.Join(parts, ", ")
}

// InstrumentError is one error reported by the instrument.
type InstrumentError struct {
	Code    int
	Message string
}

func (e InstrumentError) String() string { return fmt.Sprintf("%d,%s", e.Code, e.Message) }

// ErrorList is a non-empty set of instrument-reported errors.
type ErrorList []InstrumentError

func (l ErrorList) Error() string {
	parts := make([]string, len(l))
	for i, e := range l {
		parts[i] = e.String()
	}
	return "instrument reported: " + strings.Join(parts, "; ")
}

// ParseSystErr parses a SYST:ERR? response such as `-113,"Undefined header;FOO"`,
// `+0, No error` or `-113,Undefined header,C1:FOO `.
func ParseSystErr(r string) (InstrumentError, error) {
	r = strings.TrimSpace(r)
	codeS, msg, _ := strings.Cut(r, ",")
	code, err := strconv.Atoi(strings.TrimSpace(codeS))
	if err != nil {
		return InstrumentError{}, fmt.Errorf("unexpected SYST:ERR? response %q", r)
	}
	msg = strings.Trim(strings.TrimSpace(msg), `"`)
	return InstrumentError{Code: code, Message: strings.TrimSpace(msg)}, nil
}

// maxDrain bounds error-queue draining in case an instrument never reports 0.
const maxDrain = 32

// SCPI1999Checker drains SYST:ERR? until it reports code 0.
func SCPI1999Checker(ctx context.Context, s *Session) error {
	var errs ErrorList
	for range maxDrain {
		r, err := s.Query(ctx, "SYST:ERR?")
		if err != nil {
			return err
		}
		e, err := ParseSystErr(r)
		if err != nil {
			return err
		}
		if e.Code == 0 {
			break
		}
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// SiglentLegacyChecker is for the SDS1000X-E legacy command set, which has
// no SYST:ERR?. It reads *ESR? and, if an error bit is set, reports the
// command (CMR?) and execution (EXR?) error registers.
//
// Observed on SDS1104X-E fw 8.1.6.1.35R2: unknown headers set ESR CME (32)
// but leave CMR? at 0, and out-of-range values are silently clamped with no
// error at all, so drivers must also read back what they set.
func SiglentLegacyChecker(ctx context.Context, s *Session) error {
	esr, err := s.ESR(ctx)
	if err != nil {
		return err
	}
	if esr&esrErrorMask == 0 {
		return nil
	}
	errs := ErrorList{{Code: int(esr), Message: "ESR: " + DescribeESR(esr)}}
	for _, q := range []string{"CMR?", "EXR?"} {
		r, err := s.Query(ctx, q)
		if err != nil {
			return err
		}
		if code, err := strconv.Atoi(lastField(r)); err == nil && code != 0 {
			errs = append(errs, InstrumentError{Code: code, Message: strings.TrimSuffix(q, "?") + " register"})
		}
	}
	return errs
}
