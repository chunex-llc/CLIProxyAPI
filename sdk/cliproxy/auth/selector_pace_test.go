package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var paceTestNow = time.Unix(1_800_000_000, 0)

// observeCodexPace records a Codex secondary window for id in ledger.
func observeCodexPace(ledger *PaceLedger, id string, used float64, window time.Duration, resetAt time.Time) {
	ledger.ObserveResponseHeaders(id, "codex", http.Header{
		"X-Codex-Secondary-Used-Percent":   {strconv.FormatFloat(used, 'f', -1, 64)},
		"X-Codex-Secondary-Window-Minutes": {strconv.Itoa(int(window / time.Minute))},
		"X-Codex-Secondary-Reset-At":       {strconv.FormatInt(resetAt.Unix(), 10)},
	}, paceTestNow)
}

func codexPaceAuth(ledger *PaceLedger, id string, used float64, window time.Duration, resetAt time.Time) *Auth {
	observeCodexPace(ledger, id, used, window, resetAt)
	return &Auth{ID: id, Provider: "codex"}
}

func claudePaceAuth(ledger *PaceLedger, id, fiveHour string, fiveHourReset time.Time, sevenDay string, sevenDayReset time.Time) *Auth {
	ledger.ObserveResponseHeaders(id, "claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {fiveHour},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(fiveHourReset.Unix(), 10)},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {sevenDay},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(sevenDayReset.Unix(), 10)},
	}, paceTestNow)
	return &Auth{ID: id, Provider: "claude"}
}

func assertPaceScore(t *testing.T, ledger *PaceLedger, auth *Auth, now time.Time, want float64) {
	t.Helper()
	if got := paceScoreWindows(ledger.Windows(auth.ID, ""), now); math.Abs(got-want) > 0.01 {
		t.Fatalf("paceScore() = %.4f, want %.2f", got, want)
	}
}

func pickPace(t *testing.T, selector Selector, opts cliproxyexecutor.Options, auths []*Auth) string {
	t.Helper()
	got, err := selector.Pick(context.Background(), "codex", "", opts, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatal("Pick() auth = nil")
	}
	return got.ID
}

func TestPaceScore_CodexSingleWindow(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	week := 10080 * time.Minute
	assertPaceScore(t, ledger, codexPaceAuth(ledger, "a", 70, week, paceTestNow.Add(48*time.Hour)), paceTestNow, 105.0)
	assertPaceScore(t, ledger, codexPaceAuth(ledger, "b", 20, week, paceTestNow.Add(24*time.Hour)), paceTestNow, 560.0)
}

func TestPaceSelectorPick_PrefersBehindPace(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	now := time.Now()
	week := 10080 * time.Minute
	auths := []*Auth{
		codexPaceAuth(ledger, "a", 70, week, now.Add(48*time.Hour)),
		codexPaceAuth(ledger, "b", 20, week, now.Add(24*time.Hour)),
	}
	if got := pickPace(t, &PaceSelector{Ledger: ledger}, cliproxyexecutor.Options{}, auths); got != "b" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got, "b")
	}
}

func TestPaceScore_HarmonicMean(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	auth := claudePaceAuth(ledger, "a", "0.5", paceTestNow.Add(150*time.Minute), "0.2", paceTestNow.Add(84*time.Hour))
	assertPaceScore(t, ledger, auth, paceTestNow, 123.08)
}

func TestPaceScore_ExpiredWindowCountsFresh(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	auth := codexPaceAuth(ledger, "a", 90, 10080*time.Minute, paceTestNow.Add(-time.Minute))
	assertPaceScore(t, ledger, auth, paceTestNow, 100)
}

func TestPaceScore_NoSignals(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	assertPaceScore(t, ledger, &Auth{ID: "a", Provider: "codex"}, paceTestNow, 100)
	gemini := &Auth{ID: "b", Provider: "gemini"}
	ledger.ObserveResponseHeaders(gemini.ID, gemini.Provider, http.Header{
		"X-Codex-Secondary-Used-Percent":   {"90"},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
		"X-Codex-Secondary-Reset-At":       {strconv.FormatInt(paceTestNow.Add(time.Hour).Unix(), 10)},
	}, paceTestNow)
	assertPaceScore(t, ledger, gemini, paceTestNow, 100)
}

func TestPaceScore_ExhaustedWindowUsesMinimum(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	auth := claudePaceAuth(ledger, "a", "1.0", paceTestNow.Add(60*time.Minute), "0.1", paceTestNow.Add(84*time.Hour))
	assertPaceScore(t, ledger, auth, paceTestNow, 0)
}

func TestPaceScore_ClampsTimeShare(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	auth := codexPaceAuth(ledger, "a", 50, 300*time.Minute, paceTestNow.Add(time.Minute))
	assertPaceScore(t, ledger, auth, paceTestNow, 1000)
}

func TestPaceScore_ResetAfterSeconds(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	auth := &Auth{ID: "a", Provider: "codex"}
	ledger.ObserveResponseHeaders(auth.ID, "codex", http.Header{
		"X-Codex-Primary-Used-Percent":        {"20"},
		"X-Codex-Primary-Window-Minutes":      {"10080"},
		"X-Codex-Primary-Reset-After-Seconds": {"86400"},
	}, paceTestNow)
	assertPaceScore(t, ledger, auth, paceTestNow, 560.0)
}

func TestPaceScore_RealHeaderObservation(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	codex := &Auth{ID: "codex", Provider: "codex"}
	ledger.ObserveResponseHeaders(codex.ID, "codex", http.Header{
		"X-Codex-Primary-Used-Percent":   {"20"},
		"X-Codex-Primary-Window-Minutes": {"10080"},
		"X-Codex-Primary-Reset-At":       {strconv.FormatInt(paceTestNow.Add(24*time.Hour).Unix(), 10)},
	}, paceTestNow)
	assertPaceScore(t, ledger, codex, paceTestNow, 560.0)

	claude := &Auth{ID: "claude", Provider: "claude"}
	ledger.ObserveResponseHeaders(claude.ID, "claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.5"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(paceTestNow.Add(150*time.Minute).Unix(), 10)},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.2"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(paceTestNow.Add(84*time.Hour).Unix(), 10)},
	}, paceTestNow)
	assertPaceScore(t, ledger, claude, paceTestNow, 123.08)
}

func TestPaceSelectorPick_TiesLowestID(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	auths := []*Auth{
		{ID: "b", Provider: "codex"},
		{ID: "a", Provider: "codex"},
		{ID: "c", Provider: "codex"},
	}
	if got := pickPace(t, &PaceSelector{Ledger: ledger}, cliproxyexecutor.Options{}, auths); got != "a" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got, "a")
	}
}

func TestPaceSelectorPick_SkipsCooldown(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	now := time.Now()
	week := 10080 * time.Minute
	best := codexPaceAuth(ledger, "a", 0, week, now.Add(24*time.Hour))
	best.Unavailable = true
	best.Quota.Exceeded = true
	best.Quota.Reason = "credential_quota"
	best.Quota.NextRecoverAt = time.Now().Add(time.Hour)
	other := codexPaceAuth(ledger, "b", 90, week, now.Add(24*time.Hour))
	if got := pickPace(t, &PaceSelector{Ledger: ledger}, cliproxyexecutor.Options{}, []*Auth{best, other}); got != "b" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got, "b")
	}
}

func TestPaceSelectorPick_UnderSessionAffinity(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &PaceSelector{Ledger: ledger},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	now := time.Now()
	week := 10080 * time.Minute
	a := codexPaceAuth(ledger, "a", 70, week, now.Add(48*time.Hour))
	b := codexPaceAuth(ledger, "b", 20, week, now.Add(24*time.Hour))
	auths := []*Auth{a, b}

	session1 := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_11111111-1111-1111-1111-111111111111"}}`)}
	session2 := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_22222222-2222-2222-2222-222222222222"}}`)}

	if got := pickPace(t, selector, session1, auths); got != "b" {
		t.Fatalf("first Pick() auth.ID = %q, want %q", got, "b")
	}

	observeCodexPace(ledger, a.ID, 0, week, now.Add(48*time.Hour))
	observeCodexPace(ledger, b.ID, 95, week, now.Add(24*time.Hour))

	if got := pickPace(t, selector, session1, auths); got != "b" {
		t.Fatalf("same-session Pick() auth.ID = %q, want %q", got, "b")
	}
	if got := pickPace(t, selector, session2, auths); got != "a" {
		t.Fatalf("new-session Pick() auth.ID = %q, want %q", got, "a")
	}
}
