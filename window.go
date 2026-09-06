package main

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// WindowState classifies a normalized quota window.
type WindowState string

const (
	// WindowUnknown means no usable data was available.
	WindowUnknown WindowState = "unknown"
	// WindowKnown means sub2api sampled a window with a future reset.
	WindowKnown WindowState = "known"
	// WindowIdle means the last sampled window has ended and nothing has been
	// seen since. Anthropic anchors the 7-day window to the first message, so
	// such an account has its full quota and no deadline until it is used.
	WindowIdle WindowState = "idle"
	// WindowEstimated means the scheduler's own probe started the window and
	// no sample has arrived yet; the reset is probe time plus seven days.
	WindowEstimated WindowState = "estimated"
)

const weekly = 7 * 24 * time.Hour

// Window is one normalized Anthropic quota window.
type Window struct {
	State       WindowState `json:"state"`
	UsedPercent float64     `json:"used_percent"`
	// Reset is the deadline for known and estimated windows and, for idle
	// windows, the reset that ended the last sampled window.
	Reset     time.Time `json:"reset"`
	SampledAt time.Time `json:"sampled_at"`
}

// HasDeadline reports whether the window will expire at Reset.
func (w Window) HasDeadline() bool {
	return w.State == WindowKnown || w.State == WindowEstimated
}

// HoursToReset returns hours until the deadline, or +Inf when the window has
// none, so a caller that forgets to check the state can never treat an idle
// or unknown window as expiring.
func (w Window) HoursToReset(now time.Time) float64 {
	if !w.HasDeadline() {
		return math.Inf(1)
	}
	return w.Reset.Sub(now).Hours()
}

// NormalizeWindow converts sub2api's passive extra values into a Window.
// Utilization is always a 0-1 fraction (sub2api stores header values as-is
// and divides active values by 100); reset is unix seconds. A reset in the
// past means the window ended without a successor: idle.
func NormalizeWindow(utilization, reset, sampledAt any, now time.Time) Window {
	u, okU := toFloat(utilization)
	r, okR := toFloat(reset)
	if !okU || !okR || r <= 0 {
		return Window{State: WindowUnknown}
	}
	w := Window{State: WindowKnown, UsedPercent: u * 100, Reset: time.Unix(int64(r), 0).UTC(), SampledAt: parseSampledAt(sampledAt)}
	if !w.Reset.After(now) {
		w.State, w.UsedPercent = WindowIdle, 0
	}
	return w
}

// EstimatedWindow returns the window a probe sent at probedAt is expected to
// have started: unused, resetting seven days later rounded up to the hour,
// which is how Anthropic reports first-message-anchored windows. ok is false
// once that estimate has itself expired.
func EstimatedWindow(probedAt, now time.Time) (Window, bool) {
	end := probedAt.Add(weekly).UTC()
	if rounded := end.Truncate(time.Hour); rounded.Before(end) {
		end = rounded.Add(time.Hour)
	}
	if !end.After(now) {
		return Window{}, false
	}
	return Window{State: WindowEstimated, Reset: end, SampledAt: probedAt.UTC()}, true
}

func parseSampledAt(v any) time.Time {
	if s, ok := v.(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
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
