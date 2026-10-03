package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func paceWindowKeys(windows []paceWindow) []PaceWindowKey {
	keys := make([]PaceWindowKey, 0, len(windows))
	for _, window := range windows {
		keys = append(keys, window.key)
	}
	return keys
}

func findPaceWindow(windows []paceWindow, key PaceWindowKey) (paceWindow, bool) {
	for _, window := range windows {
		if window.key == key {
			return window, true
		}
	}
	return paceWindow{}, false
}

func TestPaceLedger_ObserveMergesPerWindow(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	ledger.Observe("a", map[PaceWindowKey]PaceObservation{
		"5h": {UsedPercent: 10, ObservedAt: paceTestNow},
		"7d": {UsedPercent: 30, ObservedAt: paceTestNow},
	})
	ledger.Observe("a", map[PaceWindowKey]PaceObservation{
		"5h": {UsedPercent: 50, ObservedAt: paceTestNow.Add(time.Minute)},
	})
	windows := ledger.Windows("a", "")
	if got := paceWindowKeys(windows); !reflect.DeepEqual(got, []PaceWindowKey{"5h", "7d"}) {
		t.Fatalf("Windows() keys = %v, want [5h 7d]", got)
	}
	if windows[0].used != 50 || windows[1].used != 30 {
		t.Fatalf("Windows() used = %v/%v, want 50/30", windows[0].used, windows[1].used)
	}
}

func TestPaceLedger_ObserveKeepsNewerWindow(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	ledger.Observe("a", map[PaceWindowKey]PaceObservation{
		"5h": {UsedPercent: 60, ObservedAt: paceTestNow},
	})
	ledger.Observe("a", map[PaceWindowKey]PaceObservation{
		"5h": {UsedPercent: 10, ObservedAt: paceTestNow.Add(-time.Minute)},
	})
	window, ok := findPaceWindow(ledger.Windows("a", ""), "5h")
	if !ok || window.used != 60 {
		t.Fatalf("5h window = %+v (found %v), want used 60", window, ok)
	}
}

func TestPaceLedger_WindowsIncludesMatchingModelWindows(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	ledger.Observe("a", map[PaceWindowKey]PaceObservation{
		"5h":          {UsedPercent: 10, ObservedAt: paceTestNow},
		"model:fable": {UsedPercent: 90, ObservedAt: paceTestNow},
	})
	if got := paceWindowKeys(ledger.Windows("a", "claude-fable-5-1")); !reflect.DeepEqual(got, []PaceWindowKey{"5h", "model:fable"}) {
		t.Fatalf("Windows(fable) keys = %v, want [5h model:fable]", got)
	}
	if got := paceWindowKeys(ledger.Windows("a", "claude-opus-5-5")); !reflect.DeepEqual(got, []PaceWindowKey{"5h"}) {
		t.Fatalf("Windows(opus) keys = %v, want [5h]", got)
	}
}

func TestPaceModelMatches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model, limit string
		want         bool
	}{
		{"claude-fable-5-1", "Fable", true},
		{"claude-opus-5-5", "Fable", false},
		{"gpt-5.3-codex-spark", "GPT-5.3 Codex Spark", true},
		{"anything", "", false},
	} {
		if got := paceModelMatches(tc.model, tc.limit); got != tc.want {
			t.Errorf("paceModelMatches(%q, %q) = %v, want %v", tc.model, tc.limit, got, tc.want)
		}
	}
}

func TestPaceSelectorPick_ModelWindowAppliesToMatchingModel(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ledger := NewPaceLedger()
	ledger.Observe("a", map[PaceWindowKey]PaceObservation{
		"5h": {UsedPercent: 10, ObservedAt: now},
		"7d": {UsedPercent: 10, ObservedAt: now},
		"model:fable": {
			UsedPercent: 95,
			ResetAt:     now.Add(72 * time.Hour),
			Duration:    168 * time.Hour,
			ObservedAt:  now,
		},
	})
	ledger.Observe("b", map[PaceWindowKey]PaceObservation{
		"5h": {UsedPercent: 40, ObservedAt: now},
		"7d": {UsedPercent: 40, ObservedAt: now},
	})
	auths := []*Auth{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}}
	selector := &PaceSelector{Ledger: ledger}
	for _, tc := range []struct{ model, want string }{
		{"claude-fable-5-1", "b"},
		{"claude-opus-5-5", "a"},
	} {
		got, err := selector.Pick(context.Background(), "claude", tc.model, cliproxyexecutor.Options{}, auths)
		if err != nil || got == nil {
			t.Fatalf("Pick(%s) auth=%v err=%v", tc.model, got, err)
		}
		if got.ID != tc.want {
			t.Fatalf("Pick(%s) auth.ID = %q, want %q", tc.model, got.ID, tc.want)
		}
	}
}

func TestPaceLedger_ResponseHeadersKeepModelWindows(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	ledger.Observe("a", map[PaceWindowKey]PaceObservation{
		"model:gpt-5.3-codex-spark": {UsedPercent: 70, ObservedAt: paceTestNow},
	})
	ledger.ObserveResponseHeaders("a", "codex", http.Header{
		"X-Codex-Primary-Used-Percent":   {"30"},
		"X-Codex-Primary-Window-Minutes": {"300"},
	}, paceTestNow.Add(time.Minute))
	windows := ledger.Windows("a", "gpt-5.3-codex-spark")
	if got := paceWindowKeys(windows); !reflect.DeepEqual(got, []PaceWindowKey{"primary", "model:gpt-5.3-codex-spark"}) {
		t.Fatalf("Windows() keys = %v, want [primary model:gpt-5.3-codex-spark]", got)
	}
	if windows[0].used != 30 || windows[0].duration != 300*time.Minute || windows[1].used != 70 {
		t.Fatalf("Windows() = %+v", windows)
	}
}

func TestPaceLedger_SnapshotRestore(t *testing.T) {
	t.Parallel()
	ledger := NewPaceLedger()
	ledger.Observe("a", map[PaceWindowKey]PaceObservation{
		"5h":          {UsedPercent: 10, ResetAt: paceTestNow.Add(time.Hour), Duration: 5 * time.Hour, ObservedAt: paceTestNow},
		"model:fable": {UsedPercent: 80, ObservedAt: paceTestNow},
	})
	ledger.Observe("b", map[PaceWindowKey]PaceObservation{
		"7d": {UsedPercent: 20, ObservedAt: paceTestNow},
	})
	restored := NewPaceLedger()
	restored.Restore(ledger.Snapshot(), func(authID string) bool { return authID != "b" })
	if got, want := restored.Windows("a", "claude-fable-5-1"), ledger.Windows("a", "claude-fable-5-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored Windows(a) = %+v, want %+v", got, want)
	}
	if got := restored.Windows("b", ""); len(got) != 0 {
		t.Fatalf("restored Windows(b) = %+v, want empty", got)
	}
}

func TestManagerMarkResultFeedsDefaultPaceLedger(t *testing.T) {
	t.Cleanup(func() {
		DefaultPaceLedger().Restore(nil, func(authID string) bool { return authID != "pace-ledger-mark-result-auth" })
	})
	manager := NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "pace-ledger-mark-result-auth",
		Provider: "claude",
	})
	if errRegister != nil || auth == nil {
		t.Fatalf("Register() auth=%#v err=%v", auth, errRegister)
	}

	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.2"},
	})
	manager.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: "claude",
		Model:    "claude-opus-5-5",
		Success:  true,
	})

	window, ok := findPaceWindow(DefaultPaceLedger().Windows(auth.ID, ""), "7d")
	if !ok || window.used != 20 {
		t.Fatalf("default ledger 7d window = %+v (found %v), want used 20", window, ok)
	}
}
