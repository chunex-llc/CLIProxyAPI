package auth

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// PaceWindowKey names one quota window of an auth: "5h" and "7d" for Claude,
// "primary" and "secondary" for Codex, and "model:<lowercased limit name>"
// (for example "model:fable") for a window that applies to matching models
// only.
type PaceWindowKey string

const paceModelWindowPrefix = "model:"

// PaceObservation is the latest observation of one quota window.
type PaceObservation struct {
	UsedPercent float64       `json:"used_percent"`       // 0–100
	ResetAt     time.Time     `json:"reset_at,omitempty"` // zero when unknown
	Duration    time.Duration `json:"duration,omitempty"` // zero when unknown
	ObservedAt  time.Time     `json:"observed_at"`
}

// PaceLedger keeps the latest observation of every quota window per auth.
// Unlike Quota.Signals, which each response replaces wholesale, it merges per
// window, so windows learned from different sources (response headers, polls,
// model-scoped limits) survive one another. It has its own lock and never
// calls into Manager, so it is safe to feed while m.mu is held.
type PaceLedger struct {
	mu      sync.RWMutex
	windows map[string]map[PaceWindowKey]PaceObservation // authID → window → latest
}

// NewPaceLedger returns an empty ledger.
func NewPaceLedger() *PaceLedger {
	return &PaceLedger{windows: make(map[string]map[PaceWindowKey]PaceObservation)}
}

var defaultPaceLedger = NewPaceLedger()

// DefaultPaceLedger is the process-wide ledger shared by the MarkResult hook
// and any PaceSelector without its own Ledger.
func DefaultPaceLedger() *PaceLedger {
	return defaultPaceLedger
}

// Observe merges obs into authID's windows. A window is replaced only when the
// new ObservedAt is not before the stored one; windows absent from obs are
// kept.
func (l *PaceLedger) Observe(authID string, obs map[PaceWindowKey]PaceObservation) {
	if l == nil || authID == "" || len(obs) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = make(map[string]map[PaceWindowKey]PaceObservation)
	}
	current := l.windows[authID]
	if current == nil {
		current = make(map[PaceWindowKey]PaceObservation, len(obs))
		l.windows[authID] = current
	}
	for key, observation := range obs {
		if stored, ok := current[key]; ok && observation.ObservedAt.Before(stored.ObservedAt) {
			continue
		}
		current[key] = observation
	}
}

// ObserveResponseHeaders records the account-wide windows carried by one
// upstream response. It never derives model windows, and headers carrying no
// window leave the ledger untouched.
func (l *PaceLedger) ObserveResponseHeaders(authID, provider string, headers http.Header, observedAt time.Time) {
	windows := paceWindowsFromSignals(provider, collectQuotaSignals(provider, headers), observedAt)
	if len(windows) == 0 {
		return
	}
	obs := make(map[PaceWindowKey]PaceObservation, len(windows))
	for _, window := range windows {
		obs[window.key] = PaceObservation{
			UsedPercent: window.used,
			ResetAt:     window.resetAt,
			Duration:    window.duration,
			ObservedAt:  observedAt,
		}
	}
	l.Observe(authID, obs)
}

// Windows returns authID's account-wide windows plus every model window whose
// limit name matches model, account-wide first, each group in key order.
func (l *PaceLedger) Windows(authID, model string) []paceWindow {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	current := l.windows[authID]
	if len(current) == 0 {
		return nil
	}
	var account, scoped []PaceWindowKey
	for key := range current {
		name, isModel := strings.CutPrefix(string(key), paceModelWindowPrefix)
		switch {
		case !isModel:
			account = append(account, key)
		case paceModelMatches(model, name):
			scoped = append(scoped, key)
		}
	}
	sort.Slice(account, func(i, j int) bool { return account[i] < account[j] })
	sort.Slice(scoped, func(i, j int) bool { return scoped[i] < scoped[j] })
	windows := make([]paceWindow, 0, len(account)+len(scoped))
	for _, key := range append(account, scoped...) {
		observation := current[key]
		windows = append(windows, paceWindow{
			key:      key,
			used:     observation.UsedPercent,
			resetAt:  observation.ResetAt,
			duration: observation.Duration,
		})
	}
	return windows
}

// Snapshot returns a deep copy of every auth's windows.
func (l *PaceLedger) Snapshot() map[string]map[PaceWindowKey]PaceObservation {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return copyPaceWindows(l.windows, nil)
}

// Restore replaces the ledger's contents with a copy of windows, dropping auths
// for which keep returns false. A nil keep keeps every auth.
func (l *PaceLedger) Restore(windows map[string]map[PaceWindowKey]PaceObservation, keep func(authID string) bool) {
	if l == nil {
		return
	}
	restored := copyPaceWindows(windows, keep)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.windows = restored
}

func copyPaceWindows(windows map[string]map[PaceWindowKey]PaceObservation, keep func(authID string) bool) map[string]map[PaceWindowKey]PaceObservation {
	out := make(map[string]map[PaceWindowKey]PaceObservation, len(windows))
	for authID, current := range windows {
		if len(current) == 0 || (keep != nil && !keep(authID)) {
			continue
		}
		copied := make(map[PaceWindowKey]PaceObservation, len(current))
		for key, observation := range current {
			copied[key] = observation
		}
		out[authID] = copied
	}
	return out
}

// paceModelMatches reports whether a model-scoped limit applies to model:
// both are lowercased and reduced to [a-z0-9], and the limit name must be a
// non-empty substring of the model.
func paceModelMatches(model, limitName string) bool {
	limit := paceAlnum(limitName)
	return limit != "" && strings.Contains(paceAlnum(model), limit)
}

func paceAlnum(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
