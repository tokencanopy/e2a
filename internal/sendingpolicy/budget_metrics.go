package sendingpolicy

import (
	"errors"
	"sync/atomic"
)

// BudgetObserver receives committed budget evaluations, never identifiers or
// recipient counts. Reserve reports only holds; Consume reports evaluated
// scopes. These are gate evaluations, not unique messages or provider sends.
type BudgetObserver func(scope, decision string)

var budgetObserver atomic.Value // BudgetObserver

// SetBudgetObserver installs the process-wide observer. nil disables it.
func SetBudgetObserver(o BudgetObserver) { budgetObserver.Store(o) }

type budgetSample struct {
	scope    Scope
	decision string
}

func emitBudgetSamples(samples []budgetSample) {
	if o, ok := budgetObserver.Load().(BudgetObserver); ok && o != nil {
		for _, s := range samples {
			o(string(s.scope), s.decision)
		}
	}
}

func observeBudgetScope(p RuntimePolicy, scope Scope) bool {
	return !(p.DisableLegacyDailyBudgets && (scope == ScopeAccountDaily || scope == ScopeAccountSharedDaily || scope == ScopeGlobalProbation))
}

func appendAccountTrustSamples(samples *[]budgetSample, st authState, err error) {
	if !st.ramp.account || !st.ramp.applies || st.ramp.units == 0 {
		return
	}
	if err == nil {
		*samples = append(*samples, budgetSample{ScopeAccountDaily, "allow"})
		if st.ramp.shared {
			*samples = append(*samples, budgetSample{ScopeAccountSharedDaily, "allow"})
		}
		return
	}
	var capacity *accountCapacityError
	if errors.As(err, &capacity) {
		scope := ScopeAccountDaily
		if capacity.daily.SharedBinding {
			scope = ScopeAccountSharedDaily
		}
		*samples = append(*samples, budgetSample{scope, "hold"})
	}
}
