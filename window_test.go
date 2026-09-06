package main

import (
	"math"
	"testing"
	"time"
)

func TestNormalizeWindowKnown(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	w := NormalizeWindow(0.32, float64(1788768000), "2026-09-03T08:41:42Z", now)
	if w.State != WindowKnown || w.UsedPercent != 32 || !w.HasDeadline() {
		t.Fatalf("got %+v", w)
	}
	if got := w.HoursToReset(now); got < 79.9 || got > 80.1 {
		t.Fatalf("hours=%v", got)
	}
	if w.SampledAt.IsZero() {
		t.Fatalf("sampled_at not parsed: %+v", w)
	}
}

func TestNormalizeWindowPastResetIsIdle(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	old := time.Date(2026, 9, 3, 23, 0, 0, 0, time.UTC)
	w := NormalizeWindow(0.37, float64(old.Unix()), "2026-09-03T08:41:42Z", now)
	if w.State != WindowIdle || w.UsedPercent != 0 || w.HasDeadline() {
		t.Fatalf("got %+v", w)
	}
	if !w.Reset.Equal(old) {
		t.Fatalf("idle window must keep the ended reset for logging: %+v", w)
	}
	if w.SampledAt.IsZero() {
		t.Fatalf("idle window must keep sampled_at: %+v", w)
	}
	if !math.IsInf(w.HoursToReset(now), 1) {
		t.Fatalf("idle window must have no deadline, got %v", w.HoursToReset(now))
	}
}

func TestNormalizeWindowUnknown(t *testing.T) {
	now := time.Unix(0, 0)
	for _, c := range []struct{ u, r any }{{nil, nil}, {0.5, nil}, {nil, 1.0}, {"x", 1.0}} {
		w := NormalizeWindow(c.u, c.r, nil, now)
		if w.State != WindowUnknown || w.HasDeadline() {
			t.Fatalf("%v/%v -> %+v", c.u, c.r, w)
		}
		if !math.IsInf(w.HoursToReset(now), 1) {
			t.Fatalf("unknown window must have no deadline, got %v", w.HoursToReset(now))
		}
	}
}

func TestNormalizeWindowPassiveIsAlwaysAFraction(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in   any
		want float64
	}{{"0.21", 21}, {0.21, 21}, {1.0, 100}, {1.03, 103}, {float64(0), 0}} {
		w := NormalizeWindow(c.in, "1788951600", nil, now)
		if w.State != WindowKnown || w.UsedPercent != c.want {
			t.Fatalf("%v -> %+v, want %v%%", c.in, w, c.want)
		}
	}
}

func TestEstimatedWindow(t *testing.T) {
	probed := time.Date(2026, 9, 6, 5, 45, 10, 0, time.UTC)
	w, ok := EstimatedWindow(probed, probed.Add(time.Minute))
	want := time.Date(2026, 9, 13, 6, 0, 0, 0, time.UTC)
	if !ok || w.State != WindowEstimated || w.UsedPercent != 0 || !w.Reset.Equal(want) || !w.HasDeadline() {
		t.Fatalf("got %+v ok=%v", w, ok)
	}
	if !w.SampledAt.Equal(probed) {
		t.Fatalf("sampled_at must be the probe time: %+v", w)
	}
	if got := w.HoursToReset(probed.Add(time.Minute)); got < 167.9 || got > 168.3 {
		t.Fatalf("hours=%v", got)
	}
	// An exact hour is not rounded further.
	onHour := time.Date(2026, 9, 6, 5, 0, 0, 0, time.UTC)
	if w, _ := EstimatedWindow(onHour, onHour); !w.Reset.Equal(onHour.Add(weekly)) {
		t.Fatalf("on-hour probe rounded wrongly: %v", w.Reset)
	}
	// Expired estimate.
	if _, ok := EstimatedWindow(probed, want); ok {
		t.Fatal("estimate at its own reset must be expired")
	}
}
