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

func TestLoadStateFrom020FileInitialisesProbes(t *testing.T) {
	dir := t.TempDir()
	old := `{"last_order":[9,11,4,8,1],"disabled_until":{},"fable_until":0,"routing_owned":false,"drain_account_id":0,"drain_until":0,"last_run":"2026-09-06T05:47:08Z"}`
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(dir)
	if err != nil || s.Probes == nil || len(s.LastOrder) != 5 {
		t.Fatalf("state=%+v err=%v", s, err)
	}
	s.Probes[8] = ProbeRecord{At: 1, OK: true, BaselineReset: 2}
	if err := SaveState(dir, s); err != nil {
		t.Fatal(err)
	}
	if again, _ := LoadState(dir); again.Probes[8].BaselineReset != 2 {
		t.Fatalf("probe record not round-tripped: %+v", again)
	}
}
