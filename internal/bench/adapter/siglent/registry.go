// Package siglent selects a driver from the *IDN? model string.
package siglent

import (
	"context"
	"strings"

	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/scpi"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/siglent/base"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/siglent/sdg1000x"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/siglent/sdm3000x"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/adapter/siglent/sds1000xe"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/domain"
	"gitlab.identitylabs.mx/ai/siglent-ctl/internal/bench/port"
)

// RoleFor returns the role implied by a model name.
func RoleFor(model string) domain.Role {
	switch {
	case strings.HasPrefix(model, "SDS"):
		return domain.RoleScope
	case strings.HasPrefix(model, "SDG"):
		return domain.RoleGenerator
	case strings.HasPrefix(model, "SDM"):
		return domain.RoleDMM
	case strings.HasPrefix(model, "SPD"):
		return domain.RolePSU
	case strings.HasPrefix(model, "SVA"), strings.HasPrefix(model, "SSA"):
		return domain.RoleAnalyzer
	}
	return domain.RoleUnknown
}

// DriverName returns the driver package that handles model, or "generic".
func DriverName(model string) string {
	switch {
	case sds1000xe.Matches(model):
		return "sds1000xe"
	case sdg1000x.Matches(model):
		return "sdg1000x"
	case sdm3000x.Matches(model):
		return "sdm3000x"
	}
	return "generic"
}

// Open wraps an identified session in the matching driver. Unsupported models
// get a generic instrument with SCPI-1999 error checking, so raw/script work.
func Open(ctx context.Context, s *scpi.Session, id domain.Identity, closer func() error) (port.Instrument, error) {
	b := base.Instrument{ID: id, S: s, Closer: closer, R: RoleFor(id.Model)}
	switch DriverName(id.Model) {
	case "sds1000xe":
		return sds1000xe.New(ctx, b)
	case "sdg1000x":
		return sdg1000x.New(ctx, b)
	case "sdm3000x":
		return sdm3000x.New(ctx, b)
	}
	s.SetChecker(scpi.SCPI1999Checker)
	return &b, nil
}
