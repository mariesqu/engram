package diagnostic

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrInvalidCheck is returned by Lookup for a code no check answers to.
var ErrInvalidCheck = errors.New("invalid diagnostic check")

// Registry is an ordered, code-keyed set of checks. The order is alphabetical
// by code so two runs of the same registry produce the same report, which is
// what makes two reports diffable.
type Registry struct {
	checks map[string]Check
	order  []string
}

// NewRegistry builds a registry from checks, ignoring nils and duplicates.
func NewRegistry(checks ...Check) Registry {
	r := Registry{checks: map[string]Check{}}
	for _, check := range checks {
		if check == nil {
			continue
		}
		code := strings.TrimSpace(check.Code())
		if code == "" {
			continue
		}
		if _, exists := r.checks[code]; !exists {
			r.order = append(r.order, code)
		}
		r.checks[code] = check
	}
	sort.Strings(r.order)
	return r
}

// DefaultRegistry is the set mem_doctor runs.
//
// Every check here answers a question about THIS schema. Upstream's registry
// carries several that cannot mean anything in this fork — manual session
// ownership modes, closed-space sync targets, quarantined pulled sessions —
// and a check that always returns ok is worse than no check: it is a line of
// reassurance about a condition nobody is testing for.
func DefaultRegistry() Registry {
	return NewRegistry(
		AmbiguousActiveSessionsCheck{},
		OrphanedObservationSessionCheck{},
		ParkedMutationsCheck{},
		ProjectPolicyUnknownCheck{},
		SessionProjectDirectoryMismatchCheck{},
		SQLiteLockContentionCheck{},
		StaleReviewBacklogCheck{},
		SyncBacklogCheck{},
	)
}

// Checks returns the registry's checks in order.
func (r Registry) Checks() []Check {
	out := make([]Check, 0, len(r.order))
	for _, code := range r.order {
		out = append(out, r.checks[code])
	}
	return out
}

// Lookup resolves one check by code.
func (r Registry) Lookup(code string) (Check, error) {
	code = strings.TrimSpace(code)
	if check, ok := r.checks[code]; ok {
		return check, nil
	}
	return nil, fmt.Errorf("%w: %q (registered: %s)", ErrInvalidCheck, code, strings.Join(RegisteredCodes(), ", "))
}

// RegisteredCodes returns every check code in the default registry — the list
// mem_doctor's description and its invalid-check error both quote, so an agent
// can always discover what it may ask for.
func RegisteredCodes() []string { return DefaultRegistry().order }
