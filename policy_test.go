package main

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 4, 0, 22, 0, 0, time.UTC)

func testConfig() *Config {
	sixty, eighty := 60.0, 80.0
	cfg := &Config{GroupID: 12, Accounts: []AccountPolicy{
		{ID: 9, Name: "relay", Kind: "relay"},
		{ID: 11, Name: "ying", Kind: "subscription"},
		{ID: 4, Name: "xb-claude", Kind: "subscription"},
		{ID: 8, Name: "my-team", Kind: "subscription"},
		{ID: 1, Name: "my-sub", Kind: "subscription", CeilingPercent: &sixty, FableCeilingPercent: &eighty, EnforceCeiling: true},
	}}
	cfg.applyDefaults()
	return cfg
}

func known(used float64, reset time.Time) Window {
	return Window{State: WindowKnown, UsedPercent: used, Reset: reset}
}

func snaps(cfg *Config, wins map[int64][2]Window, prio map[int64]int) []AccountSnapshot {
	out := []AccountSnapshot{}
	for i, a := range cfg.Accounts {
		s := AccountSnapshot{Policy: a, BaseIndex: i, Priority: prio[a.ID], Schedulable: true, Status: "active", InGroup: true}
		if w, ok := wins[a.ID]; ok {
			s.Win7d, s.WinFable = w[0], w[1]
		} else {
			s.Win7d, s.WinFable = Window{State: WindowUnknown}, Window{State: WindowUnknown}
		}
		out = append(out, s)
	}
	return out
}

func liveWindows() map[int64][2]Window {
	return map[int64][2]Window{
		11: {known(2, testNow.Add(166*time.Hour)), known(3, testNow.Add(166*time.Hour))},
		4:  {known(21, testNow.Add(131*time.Hour)), known(21, testNow.Add(131*time.Hour))},
		8:  {known(16, testNow.Add(29*time.Hour)), known(23, testNow.Add(29*time.Hour))},
		1:  {known(32, testNow.Add(80*time.Hour)), known(44, testNow.Add(80*time.Hour))},
	}
}

func livePriorities() map[int64]int { return map[int64]int{9: 1, 11: 1, 4: 2, 8: 3, 1: 100} }

func orderOf(d Decision) []int64 { return d.TargetOrder }

func TestEvaluateLiveBaselinePromotesMyTeam(t *testing.T) {
	cfg := testConfig()
	d, err := Evaluate(cfg, snaps(cfg, liveWindows(), livePriorities()), GroupRouting{}, State{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{8, 9, 11, 4, 1}
	for i, id := range want {
		if d.TargetOrder[i] != id {
			t.Fatalf("order=%v want %v", d.TargetOrder, want)
		}
	}
	// priorities 1..5 by position; only changed accounts produce actions
	got := map[int64]int{}
	for _, a := range d.Actions {
		if a.Type == "set_priority" {
			got[a.AccountID] = a.To
		}
	}
	if got[8] != 1 || got[9] != 2 || got[11] != 3 || got[4] != 4 || got[1] != 5 {
		t.Fatalf("priority actions=%v", got)
	}
	if len(d.Actions) != 5 {
		t.Fatalf("expected 5 priority actions, got %d: %+v", len(d.Actions), d.Actions)
	}
}

func TestEvaluateNoUrgencyKeepsBaseOrderAndNoWritesWhenUnchanged(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(16, testNow.Add(100*time.Hour)), known(23, testNow.Add(100*time.Hour))}
	d, _ := Evaluate(cfg, snaps(cfg, wins, map[int64]int{9: 1, 11: 2, 4: 3, 8: 4, 1: 5}), GroupRouting{}, State{}, testNow)
	if len(d.Actions) != 0 {
		t.Fatalf("expected no actions, got %+v", d.Actions)
	}
	if d.TargetOrder[0] != 9 || d.TargetOrder[4] != 1 {
		t.Fatalf("order=%v", d.TargetOrder)
	}
}

func TestEvaluateUrgentTierSortsByPressureThenReset(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	wins[4] = [2]Window{known(10, testNow.Add(48*time.Hour)), known(10, testNow.Add(48*time.Hour))} // headroom 85 / 48h = 1.77
	// my-team: headroom 79 / 29h = 2.72 -> first
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if d.TargetOrder[0] != 8 || d.TargetOrder[1] != 4 || d.TargetOrder[2] != 9 {
		t.Fatalf("order=%v", d.TargetOrder)
	}
}

func TestEvaluateHysteresisKeepsPreviousOrderForClosePressures(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(20, testNow.Add(40*time.Hour)), known(20, testNow.Add(40*time.Hour))} // 75/40 = 1.875
	wins[4] = [2]Window{known(15, testNow.Add(40*time.Hour)), known(15, testNow.Add(40*time.Hour))} // 80/40 = 2.0, within 20%
	prev := State{LastOrder: []int64{8, 4, 9, 11, 1}}
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, prev, testNow)
	if d.TargetOrder[0] != 8 || d.TargetOrder[1] != 4 {
		t.Fatalf("hysteresis violated: %v", d.TargetOrder)
	}
	wins[4] = [2]Window{known(0, testNow.Add(40*time.Hour)), known(0, testNow.Add(40*time.Hour))} // 95/40 = 2.375 > 1.875*1.2
	d, _ = Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, prev, testNow)
	if d.TargetOrder[0] != 4 {
		t.Fatalf("expected reorder: %v", d.TargetOrder)
	}
}

func TestEvaluateSmallHeadroomAndRolledAndUnknownAreNotUrgent(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(92, testNow.Add(29*time.Hour)), known(23, testNow.Add(29*time.Hour))}
	wins[11] = [2]Window{{State: WindowRolled, Reset: testNow.Add(166 * time.Hour)}, {State: WindowRolled}}
	wins[4] = [2]Window{{State: WindowUnknown}, {State: WindowUnknown}}
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	for _, a := range d.Accounts {
		if a.Urgent {
			t.Fatalf("unexpected urgent %+v", a)
		}
	}
	if d.TargetOrder[0] != 9 {
		t.Fatalf("order=%v", d.TargetOrder)
	}
}

func TestEvaluateMySubUsesItsOwnCeilingForUrgency(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	wins[1] = [2]Window{known(32, testNow.Add(70*time.Hour)), known(44, testNow.Add(70*time.Hour))} // headroom 28/70 = 0.4
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if d.TargetOrder[0] != 8 || d.TargetOrder[1] != 1 || d.TargetOrder[2] != 9 {
		t.Fatalf("order=%v", d.TargetOrder)
	}
}

func TestEvaluateReserveDisablesAndReleases(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	reset := testNow.Add(80 * time.Hour)
	wins[1] = [2]Window{known(61, reset), known(44, reset)}
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	var found *Action
	for i := range d.Actions {
		if d.Actions[i].Type == "set_schedulable" {
			found = &d.Actions[i]
		}
	}
	if found == nil || found.AccountID != 1 || found.Value != false {
		t.Fatalf("expected disable action, got %+v", d.Actions)
	}
	if d.State.DisabledUntil[1] != reset.Unix() {
		t.Fatalf("state=%+v", d.State)
	}
	// Already disabled: no duplicate action, state kept.
	ss := snaps(cfg, wins, livePriorities())
	ss[4].Schedulable = false
	d, _ = Evaluate(cfg, ss, GroupRouting{}, d.State, testNow)
	for _, a := range d.Actions {
		if a.Type == "set_schedulable" {
			t.Fatalf("duplicate disable %+v", a)
		}
	}
	// Window rolled: release only because scheduler disabled it.
	ss[4].Win7d = Window{State: WindowRolled, Reset: reset.Add(weekly)}
	d, _ = Evaluate(cfg, ss, GroupRouting{}, d.State, reset.Add(time.Minute))
	var enable *Action
	for i := range d.Actions {
		if d.Actions[i].Type == "set_schedulable" {
			enable = &d.Actions[i]
		}
	}
	if enable == nil || enable.Value != true || enable.AccountID != 1 {
		t.Fatalf("expected enable, got %+v", d.Actions)
	}
	if _, still := d.State.DisabledUntil[1]; still {
		t.Fatalf("state not cleared: %+v", d.State)
	}
}

func TestEvaluateNeverEnablesManuallyDisabledAccount(t *testing.T) {
	cfg := testConfig()
	ss := snaps(cfg, liveWindows(), livePriorities())
	ss[4].Schedulable = false
	d, _ := Evaluate(cfg, ss, GroupRouting{}, State{}, testNow)
	for _, a := range d.Actions {
		if a.Type == "set_schedulable" {
			t.Fatalf("must not touch manual state: %+v", a)
		}
	}
}

func TestEvaluateFableRoutingSetAndRestored(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	reset := testNow.Add(80 * time.Hour)
	wins[1] = [2]Window{known(32, reset), known(81, reset)}
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	var r *Action
	for i := range d.Actions {
		if d.Actions[i].Type == "set_routing" {
			r = &d.Actions[i]
		}
	}
	if r == nil || !r.Enabled || len(r.Routing["claude-fable-*"]) != 4 {
		t.Fatalf("expected routing action, got %+v", d.Actions)
	}
	for _, id := range r.Routing["claude-fable-*"] {
		if id == 1 {
			t.Fatal("my-sub must be excluded from Fable routing")
		}
	}
	if d.State.FableUntil != reset.Unix() {
		t.Fatalf("state=%+v", d.State)
	}
	// Live routing already equals ours: no action.
	live := GroupRouting{Routing: r.Routing, Enabled: true}
	d2, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), live, d.State, testNow)
	for _, a := range d2.Actions {
		if a.Type == "set_routing" {
			t.Fatalf("duplicate routing %+v", a)
		}
	}
	// Foreign routing: warn, do not touch.
	foreign := GroupRouting{Routing: map[string][]int64{"claude-opus-*": {9}}, Enabled: true}
	d3, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), foreign, State{}, testNow)
	for _, a := range d3.Actions {
		if a.Type == "set_routing" {
			t.Fatalf("must not overwrite foreign routing %+v", a)
		}
	}
	if len(d3.Warnings) == 0 {
		t.Fatal("expected warning about foreign routing")
	}
	// Window rolled: restore empty routing.
	wins[1] = [2]Window{known(32, reset.Add(weekly)), {State: WindowRolled, Reset: reset.Add(weekly)}}
	d4, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), live, d.State, reset.Add(time.Minute))
	var restore *Action
	for i := range d4.Actions {
		if d4.Actions[i].Type == "set_routing" {
			restore = &d4.Actions[i]
		}
	}
	if restore == nil || restore.Enabled || len(restore.Routing) != 0 {
		t.Fatalf("expected restore, got %+v", d4.Actions)
	}
	if d4.State.FableUntil != 0 {
		t.Fatalf("state not cleared: %+v", d4.State)
	}
}

func TestEvaluateAbortsWhenPolicyAccountMissingOrOutsideGroup(t *testing.T) {
	cfg := testConfig()
	ss := snaps(cfg, liveWindows(), livePriorities())
	if _, err := Evaluate(cfg, ss[:4], GroupRouting{}, State{}, testNow); err == nil {
		t.Fatal("expected error for missing account")
	}
	ss[2].InGroup = false
	if _, err := Evaluate(cfg, ss, GroupRouting{}, State{}, testNow); err == nil {
		t.Fatal("expected error for account outside group")
	}
}

func TestEvaluateFillsPerAccountTargetsForEveryAccount(t *testing.T) {
	cfg := testConfig()
	d, err := Evaluate(cfg, snaps(cfg, liveWindows(), livePriorities()), GroupRouting{}, State{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Accounts) != len(cfg.Accounts) {
		t.Fatalf("accounts=%d", len(d.Accounts))
	}
	seen := map[int]bool{}
	for _, a := range d.Accounts {
		if a.TargetPriority < 1 || a.TargetPriority > len(cfg.Accounts) || seen[a.TargetPriority] {
			t.Fatalf("bad target priority for %s: %+v", a.Name, a)
		}
		seen[a.TargetPriority] = true
		if a.ID == 8 && (!a.Urgent || a.TargetPriority != 1) {
			t.Fatalf("my-team should be urgent and first: %+v", a)
		}
	}
}
