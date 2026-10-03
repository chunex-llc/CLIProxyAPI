package management

import "testing"

func TestNormalizeRoutingStrategyWeightedRoundRobin(t *testing.T) {
	for _, input := range []string{"weighted-round-robin", "weightedroundrobin", "wrr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "weighted-round-robin" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want weighted-round-robin, true", input, got, ok)
		}
	}
}

func TestNormalizeRoutingStrategyPace(t *testing.T) {
	got, ok := normalizeRoutingStrategy("pace")
	if !ok || got != "pace" {
		t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want pace, true", "pace", got, ok)
	}
}
