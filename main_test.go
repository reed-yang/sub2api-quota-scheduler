package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunOnceShadowNeverWrites(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	cfg := testConfig()
	cfg.Accounts = cfg.Accounts[4:] // only my-sub, present in the fake server
	cfg.Accounts = append(cfg.Accounts, AccountPolicy{ID: 9, Name: "relay", Kind: "relay"})
	cfg.Mode = "shadow"
	dir := t.TempDir()
	now := time.Date(2026, 9, 4, 0, 22, 0, 0, time.UTC)
	d, err := runOnce(context.Background(), cfg, NewAdminClient(srv.URL, "k"), dir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Actions) == 0 {
		t.Fatalf("expected priority actions, got none: %+v", d)
	}
	for _, r := range rec {
		if r.Method != "GET" {
			t.Fatalf("shadow mode wrote: %+v", r)
		}
	}
	if s, _ := LoadState(dir); len(s.LastOrder) != 2 {
		t.Fatalf("state not saved: %+v", s)
	}
}

func TestRunOnceApplyWritesActions(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	cfg := testConfig()
	cfg.Accounts = append(cfg.Accounts[4:], AccountPolicy{ID: 9, Name: "relay", Kind: "relay"})
	cfg.Mode = "apply"
	now := time.Date(2026, 9, 4, 0, 22, 0, 0, time.UTC)
	if _, err := runOnce(context.Background(), cfg, NewAdminClient(srv.URL, "k"), t.TempDir(), true, now); err != nil {
		t.Fatal(err)
	}
	writes := 0
	for _, r := range rec {
		if r.Method == "PUT" && r.Path == "/api/v1/admin/accounts/1" {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("expected one priority write for account 1, got %d: %+v", writes, rec)
	}
}

func probeRunConfig(mode string) *Config {
	cfg := testConfig()
	cfg.Accounts = append(cfg.Accounts[4:], AccountPolicy{ID: 9, Name: "relay", Kind: "relay"})
	cfg.Accounts[0].ProbeExempt = false
	cfg.Mode = mode
	cfg.RestartIdleWindows = true
	return cfg
}

func countProbes(rec []recorded) int {
	n := 0
	for _, r := range rec {
		if r.Method == "POST" && r.Path == "/api/v1/admin/accounts/1/test" {
			n++
		}
	}
	return n
}

func TestRunOnceShadowPlansProbeButNeverSendsIt(t *testing.T) {
	var rec []recorded
	now := time.Date(2026, 9, 6, 5, 22, 0, 0, time.UTC)
	srv := newServerWithReset(t, &rec, now.Add(-24*time.Hour).Unix())
	defer srv.Close()
	d, err := runOnce(context.Background(), probeRunConfig("shadow"), NewAdminClient(srv.URL, "k"), t.TempDir(), false, now)
	if err != nil {
		t.Fatal(err)
	}
	var probe *Action
	for i := range d.Actions {
		if d.Actions[i].Type == "probe" {
			probe = &d.Actions[i]
		}
	}
	if probe == nil || probe.AccountID != 1 || probe.Result != "" {
		t.Fatalf("expected a planned probe, got %+v", d.Actions)
	}
	if countProbes(rec) != 0 {
		t.Fatalf("shadow mode sent a probe: %+v", rec)
	}
	if len(d.State.Probes) != 0 {
		t.Fatalf("shadow mode must not record probes: %+v", d.State.Probes)
	}
}

func TestRunOnceApplySendsProbeThenEstimatesWindow(t *testing.T) {
	var rec []recorded
	now := time.Date(2026, 9, 6, 5, 22, 0, 0, time.UTC)
	ended := now.Add(-24 * time.Hour)
	srv := newStatefulServer(t, &rec, ended.Unix())
	defer srv.Close()
	dir := t.TempDir()
	cfg := probeRunConfig("apply")
	d, err := runOnce(context.Background(), cfg, NewAdminClient(srv.URL, "k"), dir, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if countProbes(rec) != 1 {
		t.Fatalf("expected exactly one probe, got %d: %+v", countProbes(rec), rec)
	}
	var probe *Action
	for i := range d.Actions {
		if d.Actions[i].Type == "probe" {
			probe = &d.Actions[i]
		}
	}
	if probe == nil || !strings.HasPrefix(probe.Result, "sent; window estimated to reset at 2026-09-13T06:00:00Z") {
		t.Fatalf("probe result: %+v", probe)
	}
	if last := d.Actions[len(d.Actions)-1]; last.Type != "probe" {
		t.Fatalf("probe must run after ordering writes: %+v", d.Actions)
	}
	st, err := LoadState(dir)
	if err != nil || !st.Probes[1].OK || st.Probes[1].At != now.Unix() || st.Probes[1].BaselineReset != ended.Unix() || st.Probes[1].BaselineSampledAt == 0 {
		t.Fatalf("probe not persisted with its baseline: %+v err=%v", st.Probes, err)
	}
	for _, w := range d.Warnings {
		if strings.HasPrefix(w, "apply:") {
			t.Fatalf("unexpected apply warning: %v", d.Warnings)
		}
	}
	// Five minutes later: estimated window, no second probe, no writes at all.
	rec = rec[:0]
	d2, err := runOnce(context.Background(), cfg, NewAdminClient(srv.URL, "k"), dir, true, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if countProbes(rec) != 0 {
		t.Fatalf("second run must not probe again: %+v", rec)
	}
	for _, a := range d2.Accounts {
		if a.ID == 1 && (a.Win7d.State != WindowEstimated || a.Urgent) {
			t.Fatalf("expected estimated window: %+v", a)
		}
	}
	if len(d2.Actions) != 0 || len(d2.TargetOrder) != len(d.TargetOrder) || d2.TargetOrder[0] != d.TargetOrder[0] {
		t.Fatalf("steady state must produce no actions and the same order: %+v vs %+v", d2.Actions, d.TargetOrder)
	}
	for _, r := range rec {
		if r.Method != "GET" {
			t.Fatalf("unexpected write on steady state: %+v", r)
		}
	}
}

func TestApplySkipsProbeWhenRunContextIsDone(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Date(2026, 9, 6, 5, 22, 0, 0, time.UTC)
	d := Decision{GroupID: 12, State: State{Probes: map[int64]ProbeRecord{}}, Actions: []Action{{Type: "probe", AccountID: 1, Model: "m"}}}
	errs := Apply(ctx, NewAdminClient(srv.URL, "k"), &d, now)
	if len(errs) != 1 || len(d.State.Probes) != 0 || !strings.HasPrefix(d.Actions[0].Result, "skipped: ") || countProbes(rec) != 0 {
		t.Fatalf("errs=%v decision=%+v rec=%+v", errs, d, rec)
	}
}

func TestApplyRecordsFailedProbeForCooldown(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	now := time.Date(2026, 9, 6, 5, 22, 0, 0, time.UTC)
	d := Decision{GroupID: 12, State: State{}, Actions: []Action{{Type: "probe", AccountID: 2, Model: "m"}}}
	errs := Apply(context.Background(), NewAdminClient(srv.URL, "k"), &d, now)
	if len(errs) != 1 || d.State.Probes[2].OK || d.State.Probes[2].At != now.Unix() || !strings.HasPrefix(d.Actions[0].Result, "failed: ") {
		t.Fatalf("errs=%v decision=%+v", errs, d)
	}
}
