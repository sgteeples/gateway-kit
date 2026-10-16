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

func TestBalancersSkipUnhealthyTargets(t *testing.T) {
	for _, strategy := range []string{config.BalanceRoundRobin, config.BalanceWeightedRoundRobin} {
		t.Run(strategy, func(t *testing.T) {
			ts := targets(t, 3, 1, 1)
			b, _ := NewBalancer(strategy, ts)
			ts[0].SetHealthy(false)
			if got := sequence(t, b, 6); strings.Contains(got, "a") {
				t.Errorf("sequence = %s; unhealthy target a was picked", got)
			}
		})
	}
}

func TestWeightsHoldAmongHealthyTargets(t *testing.T) {
	ts := targets(t, 1, 3, 1)
	b, _ := NewBalancer(config.BalanceWeightedRoundRobin, ts)
	ts[2].SetHealthy(false)
	got := sequence(t, b, 8)
	if strings.Count(got, "b") != 6 || strings.Count(got, "a") != 2 {
		t.Errorf("sequence = %s, want b:a = 3:1 with c excluded", got)
	}
}

func TestBalancersFailOpenWhenAllUnhealthy(t *testing.T) {
	for _, strategy := range []string{config.BalanceRoundRobin, config.BalanceWeightedRoundRobin} {
		t.Run(strategy, func(t *testing.T) {
			ts := targets(t, 1, 1)
			b, _ := NewBalancer(strategy, ts)
			ts[0].SetHealthy(false)
			ts[1].SetHealthy(false)
			if got := sequence(t, b, 4); got != "abab" {
				t.Errorf("sequence = %s; with every target down, traffic should still go to all", got)
			}
		})
	}
}
