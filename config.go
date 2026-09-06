package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// AccountPolicy describes one policy account; slice order is the base precedence.
type AccountPolicy struct {
	ID                  int64    `json:"id"`
	Name                string   `json:"name"`
	Kind                string   `json:"kind"` // "relay" or "subscription"
	CeilingPercent      *float64 `json:"ceiling_percent,omitempty"`
	FableCeilingPercent *float64 `json:"fable_ceiling_percent,omitempty"`
	EnforceCeiling      bool     `json:"enforce_ceiling,omitempty"`
	// DrainExempt keeps the account out of drain routing. Accounts with
	// EnforceCeiling are exempt implicitly.
	DrainExempt bool `json:"drain_exempt,omitempty"`
	// ProbeExempt keeps the account out of idle-window probes, for accounts
	// whose owner starts the window by using it directly.
	ProbeExempt bool `json:"probe_exempt,omitempty"`
}

// Config is the on-disk scheduler configuration.
type Config struct {
	BaseURL                    string  `json:"base_url"`
	AdminKeyEnv                string  `json:"admin_key_env"`
	Mode                       string  `json:"mode"`
	GroupID                    int64   `json:"group_id"`
	LookaheadHours             float64 `json:"lookahead_hours"`
	MinUrgentHeadroomPercent   float64 `json:"min_urgent_headroom_percent"`
	HysteresisRatio            float64 `json:"hysteresis_ratio"`
	DefaultCeilingPercent      float64 `json:"default_ceiling_percent"`
	DefaultFableCeilingPercent float64 `json:"default_fable_ceiling_percent"`
	FableModelPattern          string  `json:"fable_model_pattern"`
	// DrainHours enables "drain mode": when a non-exempt subscription resets
	// within this many hours, group routing sends DrainModelPattern to that
	// account alone so existing sessions move onto it too. 0 disables.
	DrainHours        float64 `json:"drain_hours"`
	DrainModelPattern string  `json:"drain_model_pattern"`
	// RestartIdleWindows sends one small probe message through a subscription
	// whose 7-day window ended without a successor, so Anthropic starts the
	// next window instead of leaving the account idle behind the base order.
	RestartIdleWindows bool            `json:"restart_idle_windows"`
	ProbeModel         string          `json:"probe_model"`
	ProbeCooldownHours float64         `json:"probe_cooldown_hours"`
	Accounts           []AccountPolicy `json:"accounts"`
}

// LoadConfig reads, defaults, and validates a config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.BaseURL == "" {
		c.BaseURL = "http://127.0.0.1:8080"
	}
	if c.AdminKeyEnv == "" {
		c.AdminKeyEnv = "SUB2API_QUOTA_SCHEDULER_ADMIN_KEY"
	}
	if c.Mode == "" {
		c.Mode = "shadow"
	}
	if c.LookaheadHours == 0 {
		c.LookaheadHours = 72
	}
	if c.MinUrgentHeadroomPercent == 0 {
		c.MinUrgentHeadroomPercent = 5
	}
	if c.HysteresisRatio == 0 {
		c.HysteresisRatio = 0.2
	}
	if c.DefaultCeilingPercent == 0 {
		c.DefaultCeilingPercent = 95
	}
	if c.DefaultFableCeilingPercent == 0 {
		c.DefaultFableCeilingPercent = 95
	}
	if c.FableModelPattern == "" {
		c.FableModelPattern = "claude-fable-*"
	}
	if c.DrainModelPattern == "" {
		c.DrainModelPattern = "claude-*"
	}
	if c.ProbeModel == "" {
		c.ProbeModel = "claude-haiku-4-5-20251001"
	}
	if c.ProbeCooldownHours == 0 {
		c.ProbeCooldownHours = 6
	}
}

func (c *Config) validate() error {
	if c.Mode != "shadow" && c.Mode != "apply" {
		return fmt.Errorf("mode must be shadow or apply, got %q", c.Mode)
	}
	if c.GroupID <= 0 {
		return errors.New("group_id must be positive")
	}
	if len(c.Accounts) == 0 {
		return errors.New("accounts must not be empty")
	}
	if c.DrainHours < 0 {
		return errors.New("drain_hours must not be negative")
	}
	if c.DrainHours > 0 && c.DrainModelPattern == c.FableModelPattern {
		return errors.New("drain_model_pattern must differ from fable_model_pattern")
	}
	if c.ProbeCooldownHours < 0 {
		return errors.New("probe_cooldown_hours must not be negative")
	}
	seen := map[int64]bool{}
	for _, a := range c.Accounts {
		if a.ID <= 0 || a.Name == "" {
			return fmt.Errorf("account %+v needs id and name", a)
		}
		if a.Kind != "relay" && a.Kind != "subscription" {
			return fmt.Errorf("account %d kind must be relay or subscription", a.ID)
		}
		if seen[a.ID] {
			return fmt.Errorf("duplicate account id %d", a.ID)
		}
		seen[a.ID] = true
	}
	return nil
}

// Ceiling returns the effective 7d and Fable ceilings for an account.
func (c *Config) Ceiling(a AccountPolicy) (ceiling, fable float64) {
	ceiling, fable = c.DefaultCeilingPercent, c.DefaultFableCeilingPercent
	if a.CeilingPercent != nil {
		ceiling = *a.CeilingPercent
	}
	if a.FableCeilingPercent != nil {
		fable = *a.FableCeilingPercent
	}
	return ceiling, fable
}
