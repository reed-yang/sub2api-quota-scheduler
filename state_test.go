package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTripAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir)
	if err != nil || len(s.LastOrder) != 0 || s.DisabledUntil == nil {
		t.Fatalf("empty state: %+v err=%v", s, err)
	}
	s.LastOrder = []int64{8, 9}
	s.DisabledUntil[1] = 123
	s.FableUntil = 456
	if err := SaveState(dir, s); err != nil {
		t.Fatal(err)
	}
	back, err := LoadState(dir)
	if err != nil || back.DisabledUntil[1] != 123 || back.FableUntil != 456 || back.LastOrder[0] != 8 {
		t.Fatalf("round trip: %+v err=%v", back, err)
	}
}

func TestAppendDecisionWritesOneJSONLine(t *testing.T) {
	dir := t.TempDir()
	d := Decision{Time: time.Unix(0, 0).UTC(), Mode: "shadow", GroupID: 12, TargetOrder: []int64{9}}
	if err := AppendDecision(dir, d); err != nil {
		t.Fatal(err)
	}
	if err := AppendDecision(dir, d); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "decisions.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"target_order":[9]`) {
		t.Fatalf("log=%q", raw)
	}
}
