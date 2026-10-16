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

// RoundRobin cycles through targets in order, skipping unhealthy ones.
type RoundRobin struct {
	targets []*Target
	next    atomic.Uint64
}

func (rr *RoundRobin) Next() (*Target, error) {
	n := rr.next.Add(1) - 1
	size := uint64(len(rr.targets))
	for i := range size {
		if t := rr.targets[(n+i)%size]; t.Healthy() {
			return t, nil
		}
	}
	// Every target is marked unhealthy: fail open rather than refuse all
	// traffic, since a broken health endpoint shouldn't take the route down.
	return rr.targets[n%size], nil
}

// WeightedRoundRobin is nginx's "smooth" weighted round robin: weights 3 and 1
// yield A A B A rather than A A A B, so a heavy target never takes a burst.
// It runs over healthy targets only, so the ratio holds among those still up.
type WeightedRoundRobin struct {
	mu      sync.Mutex
	targets []*Target
	current []int
}

func newWeightedRoundRobin(targets []*Target) *WeightedRoundRobin {
	return &WeightedRoundRobin{targets: targets, current: make([]int, len(targets))}
}

func (w *WeightedRoundRobin) Next() (*Target, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i := w.pick((*Target).Healthy); i >= 0 {
		return w.targets[i], nil
	}
	// Every target is unhealthy: fail open across all of them.
	return w.targets[w.pick(func(*Target) bool { return true })], nil
}

// pick runs one round of smooth WRR over the targets that pass include and
// returns the chosen index, or -1 if none pass.
func (w *WeightedRoundRobin) pick(include func(*Target) bool) int {
	best, total := -1, 0
	for i, t := range w.targets {
		if !include(t) {
			continue
		}
		w.current[i] += t.Weight
		total += t.Weight
		if best < 0 || w.current[i] > w.current[best] {
			best = i
		}
	}
	if best >= 0 {
		w.current[best] -= total
	}
	return best
}
