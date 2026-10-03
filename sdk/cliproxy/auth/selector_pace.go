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
// its observed quota windows, so usage spreads toward whichever account has
// the most headroom left relative to the time remaining before its reset.
type PaceSelector struct{}

// paceWindow is one observed quota window. resetAt and duration are zero when
// the upstream did not report them.
type paceWindow struct {
	used     float64
	resetAt  time.Time
	duration time.Duration
}

const (
	paceMinTimeShare  = 0.05
	paceFreshScore    = 100.0
	claudeFiveHourWin = 300 * time.Minute
	claudeSevenDayWin = 10080 * time.Minute
)

// Pick filters exactly like FillFirstSelector and returns the auth with the
// highest pace score; ties go to the lowest auth ID.
func (s *PaceSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	var best *Auth
	bestScore := 0.0
	for _, candidate := range available {
		if candidate == nil {
			continue
		}
		score := paceScore(candidate, now)
		if best == nil || score > bestScore || (score == bestScore && candidate.ID < best.ID) {
			best = candidate
			bestScore = score
		}
	}
	if best == nil {
		return available[0], nil
	}
	return best, nil
}

// paceScore rates how much quota an auth has left relative to the time left in
// each window. An auth with no live windows scores as fresh (100). Higher is
// further behind even use.
func paceScore(auth *Auth, now time.Time) float64 {
	if auth == nil {
		return paceFreshScore
	}
	scores := make([]float64, 0, 4)
	timed := false
	for _, window := range paceWindows(auth.Provider, auth.Quota) {
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

// paceWindows extracts the credential-level quota windows from the passive
// signal snapshot. Additional, model-scoped, and overage limits are ignored.
func paceWindows(provider string, quota QuotaState) []paceWindow {
	if len(quota.Signals) == 0 {
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
			used, ok := signalNumber(quota.Signals, prefix+"-Utilization")
			if !ok {
				continue
			}
			windows = append(windows, paceWindow{
				used:     used * 100,
				resetAt:  signalUnixTime(quota.Signals, prefix+"-Reset"),
				duration: spec.duration,
			})
		}
	case "codex":
		for _, name := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + name
			used, ok := signalNumber(quota.Signals, prefix+"-Used-Percent")
			if !ok {
				continue
			}
			window := paceWindow{used: used, resetAt: signalUnixTime(quota.Signals, prefix+"-Reset-At")}
			if window.resetAt.IsZero() && !quota.ObservedAt.IsZero() {
				if after, okAfter := signalNumber(quota.Signals, prefix+"-Reset-After-Seconds"); okAfter {
					window.resetAt = quota.ObservedAt.Add(time.Duration(after * float64(time.Second)))
				}
			}
			if minutes, okMinutes := signalNumber(quota.Signals, prefix+"-Window-Minutes"); okMinutes && minutes > 0 {
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
