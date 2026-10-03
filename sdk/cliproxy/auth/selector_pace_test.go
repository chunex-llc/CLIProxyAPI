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

func codexPaceAuth(id string, used float64, window time.Duration, resetAt time.Time) *Auth {
	return &Auth{
		ID:       id,
		Provider: "codex",
		Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":   strconv.FormatFloat(used, 'f', -1, 64),
			"X-Codex-Secondary-Window-Minutes": strconv.Itoa(int(window / time.Minute)),
			"X-Codex-Secondary-Reset-At":       strconv.FormatInt(resetAt.Unix(), 10),
		}},
	}
}

func claudePaceAuth(id, fiveHour string, fiveHourReset time.Time, sevenDay string, sevenDayReset time.Time) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota: QuotaState{Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": fiveHour,
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(fiveHourReset.Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Utilization": sevenDay,
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(sevenDayReset.Unix(), 10),
		}},
	}
}

func assertPaceScore(t *testing.T, auth *Auth, now time.Time, want float64) {
	t.Helper()
	if got := paceScore(auth, now); math.Abs(got-want) > 0.01 {
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
	week := 10080 * time.Minute
	assertPaceScore(t, codexPaceAuth("a", 70, week, paceTestNow.Add(48*time.Hour)), paceTestNow, 105.0)
	assertPaceScore(t, codexPaceAuth("b", 20, week, paceTestNow.Add(24*time.Hour)), paceTestNow, 560.0)
}

func TestPaceSelectorPick_PrefersBehindPace(t *testing.T) {
	t.Parallel()
	now := time.Now()
	week := 10080 * time.Minute
	auths := []*Auth{
		codexPaceAuth("a", 70, week, now.Add(48*time.Hour)),
		codexPaceAuth("b", 20, week, now.Add(24*time.Hour)),
	}
	if got := pickPace(t, &PaceSelector{}, cliproxyexecutor.Options{}, auths); got != "b" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got, "b")
	}
}

func TestPaceScore_HarmonicMean(t *testing.T) {
	t.Parallel()
	auth := claudePaceAuth("a", "0.5", paceTestNow.Add(150*time.Minute), "0.2", paceTestNow.Add(84*time.Hour))
	assertPaceScore(t, auth, paceTestNow, 123.08)
}

func TestPaceScore_ExpiredWindowCountsFresh(t *testing.T) {
	t.Parallel()
	auth := codexPaceAuth("a", 90, 10080*time.Minute, paceTestNow.Add(-time.Minute))
	assertPaceScore(t, auth, paceTestNow, 100)
}

func TestPaceScore_NoSignals(t *testing.T) {
	t.Parallel()
	assertPaceScore(t, &Auth{ID: "a", Provider: "codex"}, paceTestNow, 100)
	gemini := codexPaceAuth("b", 90, 10080*time.Minute, paceTestNow.Add(time.Hour))
	gemini.Provider = "gemini"
	assertPaceScore(t, gemini, paceTestNow, 100)
}

func TestPaceScore_ExhaustedWindowUsesMinimum(t *testing.T) {
	t.Parallel()
	auth := claudePaceAuth("a", "1.0", paceTestNow.Add(60*time.Minute), "0.1", paceTestNow.Add(84*time.Hour))
	assertPaceScore(t, auth, paceTestNow, 0)
}

func TestPaceScore_ClampsTimeShare(t *testing.T) {
	t.Parallel()
	auth := codexPaceAuth("a", 50, 300*time.Minute, paceTestNow.Add(time.Minute))
	assertPaceScore(t, auth, paceTestNow, 1000)
}

func TestPaceScore_ResetAfterSeconds(t *testing.T) {
	t.Parallel()
	auth := &Auth{
		ID:       "a",
		Provider: "codex",
		Quota: QuotaState{
			ObservedAt: paceTestNow,
			Signals: map[string]string{
				"X-Codex-Primary-Used-Percent":        "20",
				"X-Codex-Primary-Window-Minutes":      "10080",
				"X-Codex-Primary-Reset-After-Seconds": "86400",
			},
		},
	}
	assertPaceScore(t, auth, paceTestNow, 560.0)
}

func TestPaceScore_RealHeaderObservation(t *testing.T) {
	t.Parallel()
	codex := &Auth{ID: "codex", Provider: "codex"}
	if !codex.Quota.ObserveResponseHeadersForProvider("codex", http.Header{
		"X-Codex-Primary-Used-Percent":   {"20"},
		"X-Codex-Primary-Window-Minutes": {"10080"},
		"X-Codex-Primary-Reset-At":       {strconv.FormatInt(paceTestNow.Add(24*time.Hour).Unix(), 10)},
	}, paceTestNow) {
		t.Fatal("codex headers were not observed")
	}
	assertPaceScore(t, codex, paceTestNow, 560.0)

	claude := &Auth{ID: "claude", Provider: "claude"}
	if !claude.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.5"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(paceTestNow.Add(150*time.Minute).Unix(), 10)},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.2"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(paceTestNow.Add(84*time.Hour).Unix(), 10)},
	}, paceTestNow) {
		t.Fatal("claude headers were not observed")
	}
	assertPaceScore(t, claude, paceTestNow, 123.08)
}

func TestPaceSelectorPick_TiesLowestID(t *testing.T) {
	t.Parallel()
	auths := []*Auth{
		{ID: "b", Provider: "codex"},
		{ID: "a", Provider: "codex"},
		{ID: "c", Provider: "codex"},
	}
	if got := pickPace(t, &PaceSelector{}, cliproxyexecutor.Options{}, auths); got != "a" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got, "a")
	}
}

func TestPaceSelectorPick_SkipsCooldown(t *testing.T) {
	t.Parallel()
	now := time.Now()
	week := 10080 * time.Minute
	best := codexPaceAuth("a", 0, week, now.Add(24*time.Hour))
	best.Unavailable = true
	best.Quota.Exceeded = true
	best.Quota.Reason = "credential_quota"
	best.Quota.NextRecoverAt = time.Now().Add(time.Hour)
	other := codexPaceAuth("b", 90, week, now.Add(24*time.Hour))
	if got := pickPace(t, &PaceSelector{}, cliproxyexecutor.Options{}, []*Auth{best, other}); got != "b" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got, "b")
	}
}

func TestPaceSelectorPick_UnderSessionAffinity(t *testing.T) {
	t.Parallel()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &PaceSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	now := time.Now()
	week := 10080 * time.Minute
	a := codexPaceAuth("a", 70, week, now.Add(48*time.Hour))
	b := codexPaceAuth("b", 20, week, now.Add(24*time.Hour))
	auths := []*Auth{a, b}

	session1 := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_11111111-1111-1111-1111-111111111111"}}`)}
	session2 := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_22222222-2222-2222-2222-222222222222"}}`)}

	if got := pickPace(t, selector, session1, auths); got != "b" {
		t.Fatalf("first Pick() auth.ID = %q, want %q", got, "b")
	}

	a.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "0"
	b.Quota.Signals["X-Codex-Secondary-Used-Percent"] = "95"

	if got := pickPace(t, selector, session1, auths); got != "b" {
		t.Fatalf("same-session Pick() auth.ID = %q, want %q", got, "b")
	}
	if got := pickPace(t, selector, session2, auths); got != "a" {
		t.Fatalf("new-session Pick() auth.ID = %q, want %q", got, "a")
	}
}
