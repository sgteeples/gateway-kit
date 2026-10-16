package upstream

import (
	"strings"
	"testing"

	"github.com/sgteeples/gateway-kit/internal/config"
)

func targets(t *testing.T, weights ...int) []*Target {
	t.Helper()
	out := make([]*Target, len(weights))
	for i, w := range weights {
		tg, err := NewTarget("http://t"+string(rune('a'+i)), w)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = tg
	}
	return out
}

func sequence(t *testing.T, b Balancer, n int) string {
	t.Helper()
	var sb strings.Builder
	for range n {
		tg, err := b.Next()
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString(strings.TrimPrefix(tg.URL.Host, "t"))
	}
	return sb.String()
}

func TestRoundRobin(t *testing.T) {
	b, err := NewBalancer(config.BalanceRoundRobin, targets(t, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := sequence(t, b, 6); got != "abcabc" {
		t.Errorf("sequence = %s, want abcabc", got)
	}
}

func TestWeightedRoundRobinIsSmooth(t *testing.T) {
	b, err := NewBalancer(config.BalanceWeightedRoundRobin, targets(t, 3, 1))
	if err != nil {
		t.Fatal(err)
	}
	// 3:1, with b interleaved rather than after a run of three a's.
	if got := sequence(t, b, 8); got != "aabaaaba" {
		t.Errorf("sequence = %s, want aabaaaba", got)
	}
}

func TestUnknownBalanceStrategy(t *testing.T) {
	if _, err := NewBalancer("random", targets(t, 1)); err == nil {
		t.Error("expected an error for an unknown strategy")
	}
}
