package upstream

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sgteeples/gateway-kit/internal/config"
)

var ErrNoTargets = errors.New("no upstream targets configured")

// Balancer chooses the target for one upstream attempt. Implementations must
// be safe for concurrent use.
type Balancer interface {
	Next() (*Target, error)
}

// NewBalancer builds the balancer named by the config's balance strategy.
func NewBalancer(strategy string, targets []*Target) (Balancer, error) {
	if len(targets) == 0 {
		return nil, ErrNoTargets
	}
	switch strategy {
	case config.BalanceRoundRobin:
		return &RoundRobin{targets: targets}, nil
	case config.BalanceWeightedRoundRobin:
		return newWeightedRoundRobin(targets), nil
	default:
		return nil, fmt.Errorf("balance strategy %q is not supported", strategy)
	}
}

// RoundRobin cycles through targets in order.
type RoundRobin struct {
	targets []*Target
	next    atomic.Uint64
}

func (rr *RoundRobin) Next() (*Target, error) {
	n := rr.next.Add(1) - 1
	return rr.targets[n%uint64(len(rr.targets))], nil
}

// WeightedRoundRobin is nginx's "smooth" weighted round robin: weights 3 and 1
// yield A A B A rather than A A A B, so a heavy target never takes a burst.
type WeightedRoundRobin struct {
	mu      sync.Mutex
	targets []*Target
	current []int
	total   int
}

func newWeightedRoundRobin(targets []*Target) *WeightedRoundRobin {
	w := &WeightedRoundRobin{targets: targets, current: make([]int, len(targets))}
	for _, t := range targets {
		w.total += t.Weight
	}
	return w
}

func (w *WeightedRoundRobin) Next() (*Target, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	best := 0
	for i, t := range w.targets {
		w.current[i] += t.Weight
		if w.current[i] > w.current[best] {
			best = i
		}
	}
	w.current[best] -= w.total
	return w.targets[best], nil
}
