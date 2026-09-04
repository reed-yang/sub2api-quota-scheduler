package main

import (
	"encoding/json"
	"strconv"
	"time"
)

// WindowState classifies a normalized quota window.
type WindowState string

const (
	WindowUnknown WindowState = "unknown"
	WindowKnown   WindowState = "known"
	WindowRolled  WindowState = "rolled"
)

const weekly = 7 * 24 * time.Hour

// Window is one normalized Anthropic quota window.
type Window struct {
	State       WindowState `json:"state"`
	UsedPercent float64     `json:"used_percent"`
	Reset       time.Time   `json:"reset"`
	SampledAt   time.Time   `json:"sampled_at"`
}

// HoursToReset returns hours until the (possibly extrapolated) reset.
func (w Window) HoursToReset(now time.Time) float64 {
	if w.State == WindowUnknown {
		return 0
	}
	return w.Reset.Sub(now).Hours()
}

// NormalizeWindow converts raw extra values into a Window. Utilization may be a
// 0-1 fraction or a percent; reset is unix seconds. A reset in the past is
// treated as a rolled window with zero usage and the reset advanced by whole
// weeks.
func NormalizeWindow(utilization, reset, sampledAt any, now time.Time) Window {
	u, okU := toFloat(utilization)
	r, okR := toFloat(reset)
	if !okU || !okR || r <= 0 {
		return Window{State: WindowUnknown}
	}
	if u <= 1 {
		u *= 100
	}
	w := Window{State: WindowKnown, UsedPercent: u, Reset: time.Unix(int64(r), 0).UTC()}
	if s, ok := sampledAt.(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			w.SampledAt = t.UTC()
		}
	}
	if !w.Reset.After(now) {
		w.State = WindowRolled
		w.UsedPercent = 0
		for !w.Reset.After(now) {
			w.Reset = w.Reset.Add(weekly)
		}
	}
	return w
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	default:
		return 0, false
	}
}
