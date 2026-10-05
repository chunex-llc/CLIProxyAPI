package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// PaceSelector picks the available auth that is furthest behind even use of
// its observed quota windows, read from the merged per-window PaceLedger that
// upstream response headers and the usage poller both feed, so allowance that
// would expire unused is spent first. It is a custom selector, not a built-in one, and is meant to
// run as the session-affinity fallback: there it decides only new sessions,
// expired bindings and failover, and the affinity wrapper hands it already
// validated candidates. That placement is also why a Reservation never moves a
// session that is already bound.
type PaceSelector struct {
	// Ledger supplies the observed windows; nil uses DefaultPaceLedger.
	Ledger *PaceLedger
	// Reservation, when set, keeps the tail of each weekly window for the
	// account owner's direct use; nil reserves nothing.
	Reservation *PaceReservation
}

// PaceReservation holds back the last ReservePercent of every weekly window
// from new selections until ReleaseBeforeReset before that window resets.
type PaceReservation struct {
	ReservePercent     float64
	ReleaseBeforeReset time.Duration
}

func (s *PaceSelector) ledger() *PaceLedger {
	if s != nil && s.Ledger != nil {
		return s.Ledger
	}
	return DefaultPaceLedger()
}

// paceWindow is one observed quota window. resetAt and duration are zero when
// the upstream did not report them.
type paceWindow struct {
	key      PaceWindowKey
	used     float64
	resetAt  time.Time
	duration time.Duration
}

const (
	// paceMinTimeShare floors a window's remaining time share so a window about
	// to reset scores at most 20x its remaining allowance instead of infinity.
	paceMinTimeShare  = 0.05
	paceFreshScore    = 100.0
	claudeFiveHourWin = 300 * time.Minute
	claudeSevenDayWin = 10080 * time.Minute
	// paceWeek is the shortest window a reservation applies to.
	paceWeek = 7 * 24 * time.Hour
)

// Pick filters like FillFirstSelector and returns the auth with the highest
// pace score; ties go to the lowest auth ID, matching fill-first's order.
func (s *PaceSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	ledger := s.ledger()
	var best *Auth
	bestScore := 0.0
	var release time.Time
	for _, candidate := range available {
		if candidate == nil {
			continue
		}
		windows := ledger.Windows(candidate.ID, model)
		if releaseAt, reserved := s.Reservation.reservedUntil(windows, now); reserved {
			if release.IsZero() || releaseAt.Before(release) {
				release = releaseAt
			}
			continue
		}
		score := paceScoreWindows(windows, now)
		if best == nil || score > bestScore || (score == bestScore && candidate.ID < best.ID) {
			best = candidate
			bestScore = score
		}
	}
	if best == nil && !release.IsZero() {
		// Every candidate is reserved: never spend the reserve as a fallback.
		return nil, newAuthUnavailableError(release, now)
	}
	if best == nil {
		return available[0], nil
	}
	return best, nil
}

// reservedUntil reports whether any weekly window is inside its reserve and
// not yet within ReleaseBeforeReset of resetting, and when the last such window
// is released. A window without a known duration or reset never reserves.
func (r *PaceReservation) reservedUntil(windows []paceWindow, now time.Time) (time.Time, bool) {
	var until time.Time
	if r == nil {
		return until, false
	}
	for _, window := range windows {
		if window.duration < paceWeek {
			continue
		}
		releaseAt := window.resetAt.Add(-r.ReleaseBeforeReset)
		if releaseAt.After(now) && window.used >= 100-r.ReservePercent && releaseAt.After(until) {
			until = releaseAt
		}
	}
	return until, !until.IsZero()
}

// paceScoreWindows rates how much quota an auth has left relative to the time
// left in each of its windows. An auth with no live windows scores as fresh (100). Higher is
// further behind even use. Several timed windows combine by harmonic mean so
// the tightest window dominates without ignoring the others; once any window
// is exhausted (score <= 0) the minimum wins so the auth sorts last.
func paceScoreWindows(windows []paceWindow, now time.Time) float64 {
	scores := make([]float64, 0, len(windows))
	timed := false
	for _, window := range windows {
		if !window.resetAt.IsZero() && !window.resetAt.After(now) {
			continue
		}
		left := 100 - window.used
		if !window.resetAt.IsZero() && window.duration > 0 {
			share := float64(window.resetAt.Sub(now)) / float64(window.duration)
			share = math.Min(math.Max(share, paceMinTimeShare), 1)
			scores = append(scores, left/share)
			timed = true
			continue
		}
		scores = append(scores, left)
	}
	if len(scores) == 0 {
		return paceFreshScore
	}
	allPositive := true
	minScore := scores[0]
	for _, score := range scores {
		if score <= 0 {
			allPositive = false
		}
		minScore = math.Min(minScore, score)
	}
	if !timed || !allPositive {
		return minScore
	}
	inverseSum := 0.0
	for _, score := range scores {
		inverseSum += 1 / score
	}
	return float64(len(scores)) / inverseSum
}

// paceWindowsFromSignals extracts the credential-level quota windows from one
// passive signal snapshot observed at observedAt. Additional, model-scoped, and
// overage limits are ignored.
func paceWindowsFromSignals(provider string, signals map[string]string, observedAt time.Time) []paceWindow {
	if len(signals) == 0 {
		return nil
	}
	var windows []paceWindow
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		for _, spec := range []struct {
			name     string
			duration time.Duration
		}{{"5h", claudeFiveHourWin}, {"7d", claudeSevenDayWin}} {
			prefix := "Anthropic-Ratelimit-Unified-" + spec.name
			used, ok := signalNumber(signals, prefix+"-Utilization")
			if !ok {
				continue
			}
			windows = append(windows, paceWindow{
				key:      PaceWindowKey(spec.name),
				used:     used * 100,
				resetAt:  signalUnixTime(signals, prefix+"-Reset"),
				duration: spec.duration,
			})
		}
	case "codex":
		for _, name := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + name
			used, ok := signalNumber(signals, prefix+"-Used-Percent")
			if !ok {
				continue
			}
			window := paceWindow{
				key:     PaceWindowKey(strings.ToLower(name)),
				used:    used,
				resetAt: signalUnixTime(signals, prefix+"-Reset-At"),
			}
			if window.resetAt.IsZero() && !observedAt.IsZero() {
				if after, okAfter := signalNumber(signals, prefix+"-Reset-After-Seconds"); okAfter {
					window.resetAt = observedAt.Add(time.Duration(after * float64(time.Second)))
				}
			}
			if minutes, okMinutes := signalNumber(signals, prefix+"-Window-Minutes"); okMinutes && minutes > 0 {
				window.duration = time.Duration(minutes * float64(time.Minute))
			}
			windows = append(windows, window)
		}
	}
	return windows
}

// signalValue looks up a signal by header name, ignoring case.
func signalValue(signals map[string]string, name string) (string, bool) {
	if value, ok := signals[name]; ok {
		return value, true
	}
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}

// signalNumber parses a finite numeric signal.
func signalNumber(signals map[string]string, name string) (float64, bool) {
	raw, ok := signalValue(signals, name)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// signalUnixTime parses an integer Unix-seconds signal, or returns zero.
func signalUnixTime(signals map[string]string, name string) time.Time {
	raw, ok := signalValue(signals, name)
	if !ok {
		return time.Time{}
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}
