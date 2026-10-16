// Package config loads, validates and normalizes the GatewayKit YAML config.
//
// The structs mirror the whole schema, including features the gateway may not
// enforce yet. That lets the decoder reject unknown keys (almost always typos)
// without rejecting valid configs.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Enumerated config values.
const (
	StrategyFixedWindow   = "fixed_window"
	StrategySlidingWindow = "sliding_window"

	PerIP     = "ip"
	PerGlobal = "global"

	BalanceRoundRobin         = "round_robin"
	BalanceWeightedRoundRobin = "weighted_round_robin"

	BackoffFixed       = "fixed"
	BackoffExponential = "exponential"

	AuthAPIKey = "api_key"
)

type Config struct {
	Gateway Gateway `yaml:"gateway"`
	Routes  []Route `yaml:"routes"`
}

type Gateway struct {
	Port            int        `yaml:"port"`
	GlobalTimeout   Duration   `yaml:"global_timeout"`
	GlobalRateLimit *RateLimit `yaml:"global_rate_limit"`
}

type Route struct {
	Path              string          `yaml:"path"`
	Methods           []string        `yaml:"methods"`
	StripPrefix       bool            `yaml:"strip_prefix"`
	Upstream          Upstream        `yaml:"upstream"`
	Retry             *Retry          `yaml:"retry"`
	RateLimit         *RateLimit      `yaml:"rate_limit"`
	HealthCheck       *HealthCheck    `yaml:"health_check"`
	RequestTransform  *Transform      `yaml:"request_transform"`
	ResponseTransform *Transform      `yaml:"response_transform"`
	Auth              *Auth           `yaml:"auth"`
	CircuitBreaker    *CircuitBreaker `yaml:"circuit_breaker"`
}

// Upstream accepts either a single url or a list of weighted targets. After
// normalization Targets is always populated and URL can be ignored.
type Upstream struct {
	URL     string   `yaml:"url"`
	Targets []Target `yaml:"targets"`
	Balance string   `yaml:"balance"`
	Timeout Duration `yaml:"timeout"`
}

type Target struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

type Retry struct {
	Attempts     int      `yaml:"attempts"`
	Backoff      string   `yaml:"backoff"`
	InitialDelay Duration `yaml:"initial_delay"`
	On           []int    `yaml:"on"`
}

type RateLimit struct {
	Requests int      `yaml:"requests"`
	Window   Duration `yaml:"window"`
	Strategy string   `yaml:"strategy"`
	Per      string   `yaml:"per"`
}

type HealthCheck struct {
	Path               string   `yaml:"path"`
	Interval           Duration `yaml:"interval"`
	UnhealthyThreshold int      `yaml:"unhealthy_threshold"`
}

type Transform struct {
	Headers *HeaderTransform `yaml:"headers"`
	Body    *BodyTransform   `yaml:"body"`
}

type HeaderTransform struct {
	Add    map[string]string `yaml:"add"`
	Remove []string          `yaml:"remove"`
}

// BodyTransform restructures a JSON body. Mapping builds a new object from
// "destination.path: source.path" pairs; Envelope wraps the body in a template
// where the string "$body" marks where the original goes.
type BodyTransform struct {
	Mapping  map[string]string `yaml:"mapping"`
	Envelope map[string]any    `yaml:"envelope"`
}

type Auth struct {
	Type   string   `yaml:"type"`
	Header string   `yaml:"header"`
	Keys   []string `yaml:"keys"`
}

type CircuitBreaker struct {
	Threshold int      `yaml:"threshold"`
	Window    Duration `yaml:"window"`
	Cooldown  Duration `yaml:"cooldown"`
}

// Load reads, validates and normalizes the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("invalid config %s:\n%w", path, err)
	}
	return cfg, nil
}

// Parse decodes YAML, validates it, then fills in defaults. Validation runs
// first so error messages refer to what the user actually wrote.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	return &cfg, nil
}
