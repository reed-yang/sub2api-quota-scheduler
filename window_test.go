package main

import (
	"testing"
	"time"
)

func TestNormalizeWindowKnown(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	w := NormalizeWindow(0.32, float64(1788768000), "2026-09-03T08:41:42Z", now)
	if w.State != WindowKnown || w.UsedPercent != 32 {
		t.Fatalf("got %+v", w)
	}
	if got := w.HoursToReset(now); got < 79.9 || got > 80.1 {
		t.Fatalf("hours=%v", got)
	}
}

func TestNormalizeWindowRolledAdvancesWholeWeeks(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	old := time.Date(2026, 9, 3, 23, 0, 0, 0, time.UTC).Unix()
	w := NormalizeWindow(0.37, float64(old), nil, now)
	if w.State != WindowRolled || w.UsedPercent != 0 {
		t.Fatalf("got %+v", w)
	}
	want := time.Date(2026, 9, 10, 23, 0, 0, 0, time.UTC)
	if !w.Reset.Equal(want) {
		t.Fatalf("reset=%v want %v", w.Reset, want)
	}
}

func TestNormalizeWindowUnknown(t *testing.T) {
	now := time.Unix(0, 0)
	for _, c := range []struct{ u, r any }{{nil, nil}, {0.5, nil}, {nil, 1.0}, {"x", 1.0}} {
		if w := NormalizeWindow(c.u, c.r, nil, now); w.State != WindowUnknown {
			t.Fatalf("%v/%v -> %+v", c.u, c.r, w)
		}
	}
}

func TestNormalizeWindowAcceptsStringAndPercentInputs(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	w := NormalizeWindow("0.21", "1788951600", nil, now)
	if w.State != WindowKnown || w.UsedPercent != 21 {
		t.Fatalf("got %+v", w)
	}
	w = NormalizeWindow(44.0, float64(1788951600), nil, now)
	if w.UsedPercent != 44 {
		t.Fatalf("percent input got %+v", w)
	}
}
