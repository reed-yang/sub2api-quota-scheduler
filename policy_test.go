package main

import (
	"strings"
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
	wins[4] = [2]Window{known(10, testNow.Add(48*time.Hour)), known(10, testNow.Add(48*time.Hour))} // headroom 90 / 48h = 1.875
	// my-team: headroom 84 / 29h = 2.90 -> first
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if d.TargetOrder[0] != 8 || d.TargetOrder[1] != 4 || d.TargetOrder[2] != 9 {
		t.Fatalf("order=%v", d.TargetOrder)
	}
}

func TestEvaluateHysteresisKeepsPreviousOrderForClosePressures(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(20, testNow.Add(40*time.Hour)), known(20, testNow.Add(40*time.Hour))} // 80/40 = 2.0
	wins[4] = [2]Window{known(15, testNow.Add(40*time.Hour)), known(15, testNow.Add(40*time.Hour))} // 85/40 = 2.125, within 20%
	prev := State{LastOrder: []int64{8, 4, 9, 11, 1}}
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, prev, testNow)
	if d.TargetOrder[0] != 8 || d.TargetOrder[1] != 4 {
		t.Fatalf("hysteresis violated: %v", d.TargetOrder)
	}
	wins[4] = [2]Window{known(0, testNow.Add(40*time.Hour)), known(0, testNow.Add(40*time.Hour))} // 100/40 = 2.5 > 2.0*1.2
	d, _ = Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, prev, testNow)
	if d.TargetOrder[0] != 4 {
		t.Fatalf("expected reorder: %v", d.TargetOrder)
	}
}

func TestEvaluateSmallHeadroomAndIdleAndUnknownAreNotUrgent(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(97, testNow.Add(29*time.Hour)), known(23, testNow.Add(29*time.Hour))}
	wins[11] = [2]Window{{State: WindowIdle, Reset: testNow.Add(-2 * time.Hour)}, {State: WindowIdle}}
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

func TestFinalWindowPriorityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		remaining   time.Duration
		used        float64
		state       WindowState
		schedulable bool
		status      string
		wantFirst   bool
	}{
		{"five hours", 5 * time.Hour, 99, WindowKnown, true, "active", true},
		{"just outside", 5*time.Hour + time.Second, 99, WindowKnown, true, "active", false},
		{"almost reset", time.Second, 99.9, WindowKnown, true, "active", true},
		{"at reset", 0, 99, WindowKnown, true, "active", false},
		{"past reset", -time.Second, 99, WindowKnown, true, "active", false},
		{"exhausted", time.Hour, 100, WindowKnown, true, "active", false},
		{"idle", time.Hour, 0, WindowIdle, true, "active", false},
		{"unknown", time.Hour, 0, WindowUnknown, true, "active", false},
		{"estimated", time.Hour, 0, WindowEstimated, true, "active", true},
		{"disabled", time.Hour, 99, WindowKnown, false, "active", false},
		{"error", time.Hour, 99, WindowKnown, true, "error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.LookaheadHours = 0.5
			wins := liveWindows()
			wins[8] = [2]Window{{State: tc.state, UsedPercent: tc.used, Reset: testNow.Add(tc.remaining)}, known(100, testNow.Add(time.Hour))}
			ss := snaps(cfg, wins, livePriorities())
			ss[3].Schedulable, ss[3].Status = tc.schedulable, tc.status
			d, err := Evaluate(cfg, ss, GroupRouting{}, State{LastOrder: []int64{9, 11, 4, 1, 8}}, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if (d.TargetOrder[0] == 8) != tc.wantFirst {
				t.Fatalf("order=%v wantFirst=%v", d.TargetOrder, tc.wantFirst)
			}
			for _, a := range d.Actions {
				if a.Type == "set_schedulable" || a.Type == "set_routing" {
					t.Fatalf("priority promotion must not change availability or routing: %+v", a)
				}
			}
		})
	}
}

func TestFinalWindowOutranksPressureAndHysteresis(t *testing.T) {
	cfg := testConfig()
	wins := liveWindows()
	wins[11] = [2]Window{known(0, testNow.Add(6*time.Hour)), {State: WindowUnknown}}
	wins[4] = [2]Window{known(0, testNow.Add(5*time.Hour)), {State: WindowUnknown}}
	wins[8] = [2]Window{known(99, testNow.Add(4*time.Hour)), known(100, testNow.Add(4*time.Hour))}
	d, err := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{LastOrder: []int64{11, 4, 8, 9, 1}}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.TargetOrder[0] != 8 || d.TargetOrder[1] != 4 || d.TargetOrder[2] != 11 {
		t.Fatalf("final window must outrank pressure and previous order: %v", d.TargetOrder)
	}
}

func TestFinalWindowPreservesPersonal95PercentReserve(t *testing.T) {
	for _, used := range []float64{94.9, 95, 99} {
		cfg := testConfig()
		limit := 95.0
		cfg.Accounts[4].CeilingPercent, cfg.Accounts[4].FableCeilingPercent = &limit, &limit
		wins := liveWindows()
		wins[1] = [2]Window{known(used, testNow.Add(time.Hour)), known(95, testNow.Add(time.Hour))}
		d, err := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if (d.TargetOrder[0] == 1) != (used < 95) {
			t.Fatalf("used=%v order=%v", used, d.TargetOrder)
		}
		disabled := false
		for _, a := range d.Actions {
			if a.Type == "set_schedulable" && a.AccountID == 1 && !a.Value {
				disabled = true
			}
		}
		if disabled != (used >= 95) {
			t.Fatalf("used=%v disabled=%v", used, disabled)
		}
		r := routingAction(d)
		if r == nil || len(r.Routing[cfg.FableModelPattern]) != 4 {
			t.Fatalf("Fable reserve must remain active: %+v", d.Actions)
		}
		for _, id := range r.Routing[cfg.FableModelPattern] {
			if id == 1 {
				t.Fatal("personal reserve must exclude Fable traffic")
			}
		}
	}
}

func TestDrainConsumesUnreservedRemainderWithFableExhausted(t *testing.T) {
	cfg := drainConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(99, testNow.Add(3*time.Hour)), known(100, testNow.Add(3*time.Hour))}
	d, err := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if d.DrainAccount != 8 || d.TargetOrder[0] != 8 {
		t.Fatalf("remaining 7d quota must stay available for other models: %+v", d)
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
	// Window ended with no successor: release only because scheduler disabled it.
	ss[4].Win7d = Window{State: WindowIdle, Reset: reset}
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
	// Fable window ended with no successor: restore empty routing.
	wins[1] = [2]Window{known(32, reset.Add(weekly)), {State: WindowIdle, Reset: reset}}
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

func drainConfig() *Config {
	cfg := testConfig()
	cfg.DrainHours = 24
	return cfg
}

func routingAction(d Decision) *Action {
	for i := range d.Actions {
		if d.Actions[i].Type == "set_routing" {
			return &d.Actions[i]
		}
	}
	return nil
}

func TestDrainDisabledByDefaultProducesNoRouting(t *testing.T) {
	cfg := testConfig()
	d, err := Evaluate(cfg, snaps(cfg, liveWindows(), livePriorities()), GroupRouting{}, State{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if routingAction(d) != nil || d.DrainAccount != 0 {
		t.Fatalf("drain must be off by default: %+v", d.Actions)
	}
}

func TestDrainRoutesEverythingToSoonestNonExemptAccount(t *testing.T) {
	cfg := drainConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(25, testNow.Add(21*time.Hour)), known(41, testNow.Add(21*time.Hour))}
	wins[1] = [2]Window{known(20, testNow.Add(10*time.Hour)), known(20, testNow.Add(10*time.Hour))} // my-sub sooner but exempt
	d, err := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	r := routingAction(d)
	if r == nil || !r.Enabled || len(r.Routing) != 1 || len(r.Routing["claude-*"]) != 1 || r.Routing["claude-*"][0] != 8 {
		t.Fatalf("expected claude-* -> [8], got %+v", d.Actions)
	}
	if d.DrainAccount != 8 || d.State.DrainAccountID != 8 || !d.State.RoutingOwned || d.State.DrainUntil != wins[8][0].Reset.Unix() {
		t.Fatalf("state=%+v drain=%d", d.State, d.DrainAccount)
	}
}

func TestDrainIgnoresAccountsOutsideWindowUnschedulableOrExhausted(t *testing.T) {
	cfg := drainConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(25, testNow.Add(30*time.Hour)), known(25, testNow.Add(30*time.Hour))} // outside 24h
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if routingAction(d) != nil {
		t.Fatalf("no account within 24h: %+v", d.Actions)
	}
	wins[8] = [2]Window{known(100, testNow.Add(3*time.Hour)), known(25, testNow.Add(3*time.Hour))} // exhausted
	d, _ = Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if routingAction(d) != nil {
		t.Fatalf("exhausted account must not be drained: %+v", d.Actions)
	}
	wins[8] = [2]Window{known(25, testNow.Add(3*time.Hour)), known(25, testNow.Add(3*time.Hour))}
	ss := snaps(cfg, wins, livePriorities())
	ss[3].Schedulable = false
	d, _ = Evaluate(cfg, ss, GroupRouting{}, State{}, testNow)
	if routingAction(d) != nil {
		t.Fatalf("unschedulable account must not be drained: %+v", d.Actions)
	}
	cfg.Accounts[3].DrainExempt = true
	d, _ = Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if routingAction(d) != nil {
		t.Fatalf("drain_exempt account must not be drained: %+v", d.Actions)
	}
}

func TestDrainReleasedAfterRolloverAndOnlyWhenOwned(t *testing.T) {
	cfg := drainConfig()
	live := GroupRouting{Routing: map[string][]int64{"claude-*": {8}}, Enabled: true}
	wins := liveWindows()
	wins[8] = [2]Window{{State: WindowIdle, Reset: testNow.Add(-time.Hour)}, {State: WindowIdle, Reset: testNow.Add(-time.Hour)}}
	owned := State{RoutingOwned: true, DrainAccountID: 8, DrainUntil: testNow.Add(-time.Hour).Unix()}
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), live, owned, testNow)
	r := routingAction(d)
	if r == nil || r.Enabled || len(r.Routing) != 0 || d.State.RoutingOwned || d.State.DrainAccountID != 0 {
		t.Fatalf("expected routing cleared, got %+v state=%+v", d.Actions, d.State)
	}
	// Same live routing but not owned by us: warn, do not touch.
	d, _ = Evaluate(cfg, snaps(cfg, wins, livePriorities()), live, State{}, testNow)
	if routingAction(d) != nil || len(d.Warnings) == 0 {
		t.Fatalf("foreign routing must be left alone: %+v %v", d.Actions, d.Warnings)
	}
}

func TestDrainCombinesWithFableReserve(t *testing.T) {
	cfg := drainConfig()
	wins := liveWindows()
	wins[8] = [2]Window{known(25, testNow.Add(21*time.Hour)), known(41, testNow.Add(21*time.Hour))}
	wins[1] = [2]Window{known(32, testNow.Add(80*time.Hour)), known(81, testNow.Add(80*time.Hour))}
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	r := routingAction(d)
	if r == nil || len(r.Routing) != 2 || r.Routing["claude-*"][0] != 8 || len(r.Routing["claude-fable-*"]) != 4 {
		t.Fatalf("expected drain + fable routing, got %+v", d.Actions)
	}
	for _, id := range r.Routing["claude-fable-*"] {
		if id == 1 {
			t.Fatal("reserved account must stay out of Fable routing during drain")
		}
	}
	// Second run with identical live routing and owned state: no action.
	live := GroupRouting{Routing: r.Routing, Enabled: true}
	d2, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), live, d.State, testNow)
	if routingAction(d2) != nil {
		t.Fatalf("idempotency violated: %+v", d2.Actions)
	}
}

func probeConfig() *Config {
	cfg := testConfig()
	cfg.RestartIdleWindows = true
	cfg.Accounts[4].ProbeExempt = true // my-sub: the owner starts that window
	return cfg
}

var idleEnded = testNow.Add(-24 * time.Hour)

func idleWindows() map[int64][2]Window {
	wins := liveWindows()
	wins[8] = [2]Window{{State: WindowIdle, Reset: idleEnded, SampledAt: idleEnded.Add(-3 * time.Hour)}, {State: WindowIdle, Reset: idleEnded}}
	return wins
}

// probedState is a delivered probe on my-team whose baseline is idleWindows().
func probedState(at time.Time) State {
	return State{Probes: map[int64]ProbeRecord{8: {At: at.Unix(), OK: true, BaselineReset: idleEnded.Unix(), BaselineSampledAt: idleEnded.Add(-3 * time.Hour).Unix()}}}
}

func probeActions(d Decision) []Action {
	var out []Action
	for _, a := range d.Actions {
		if a.Type == "probe" {
			out = append(out, a)
		}
	}
	return out
}

func TestIdleWindowIsNeverUrgentOrDrained(t *testing.T) {
	cfg := drainConfig()
	wins := idleWindows()
	d, err := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range d.Accounts {
		if a.ID == 8 && (a.Urgent || a.Win7d.State != WindowIdle) {
			t.Fatalf("idle account must not be urgent: %+v", a)
		}
	}
	if d.DrainAccount == 8 {
		t.Fatalf("idle account must not be drained: %+v", d)
	}
	if got := probeActions(d); len(got) != 0 {
		t.Fatalf("probes are off by default, got %+v", got)
	}
}

func TestPlanProbesTargetsIdleActiveSchedulableNonExemptSubscriptions(t *testing.T) {
	cfg := probeConfig()
	wins := idleWindows()
	// my-sub idle too, but probe_exempt.
	wins[1] = [2]Window{{State: WindowIdle, Reset: testNow.Add(-time.Hour)}, {State: WindowIdle}}
	d, err := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	got := probeActions(d)
	if len(got) != 1 || got[0].AccountID != 8 || got[0].Model != cfg.ProbeModel || got[0].Result != "" {
		t.Fatalf("expected one probe for my-team, got %+v", got)
	}
	if d.Actions[len(d.Actions)-1].Type != "probe" {
		t.Fatalf("probe must be the last action: %+v", d.Actions)
	}
	// Has a deadline, not active, or unschedulable: no probe.
	for name, mutate := range map[string]func(ss []AccountSnapshot){
		"known":        func(ss []AccountSnapshot) { ss[3].Win7d = known(10, testNow.Add(100*time.Hour)) },
		"error status": func(ss []AccountSnapshot) { ss[3].Status = "error" },
		"disabled":     func(ss []AccountSnapshot) { ss[3].Schedulable = false },
	} {
		ss := snaps(cfg, idleWindows(), livePriorities())
		mutate(ss)
		d, _ := Evaluate(cfg, ss, GroupRouting{}, State{}, testNow)
		if got := probeActions(d); len(got) != 0 {
			t.Fatalf("%s: unexpected probe %+v", name, got)
		}
	}
	// Never sampled at all: probed like idle, with a distinct reason.
	ss := snaps(cfg, idleWindows(), livePriorities())
	ss[3].Win7d = Window{State: WindowUnknown}
	d, _ = Evaluate(cfg, ss, GroupRouting{}, State{}, testNow)
	if got := probeActions(d); len(got) != 1 || !strings.Contains(got[0].Reason, "no 7d sample") {
		t.Fatalf("unknown window must be probed: %+v", d.Actions)
	}
	// Idle accounts are logged with no deadline and full headroom.
	d, _ = Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, State{}, testNow)
	for _, a := range d.Accounts {
		if a.ID == 8 && (a.HoursToReset != nil || a.Headroom != 100) {
			t.Fatalf("idle decision fields: %+v", a)
		}
		if a.ID == 9 && (a.HoursToReset != nil || a.Headroom != 0) {
			t.Fatalf("relay decision fields: %+v", a)
		}
	}
}

func TestPlanProbesCapsPerRun(t *testing.T) {
	cfg := probeConfig()
	cfg.Accounts[4].ProbeExempt = false
	cfg.Accounts = append(cfg.Accounts, AccountPolicy{ID: 5, Name: "sub-e", Kind: "subscription"})
	wins := idleWindows()
	for _, id := range []int64{11, 4, 1, 5} {
		wins[id] = [2]Window{{State: WindowIdle, Reset: idleEnded}, {State: WindowIdle}}
	}
	d, err := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, State{}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := probeActions(d); len(got) != maxProbesPerRun {
		t.Fatalf("expected %d probes, got %+v", maxProbesPerRun, got)
	}
	if len(d.Warnings) == 0 || !strings.Contains(d.Warnings[len(d.Warnings)-1], "probe cap") {
		t.Fatalf("expected cap warning: %v", d.Warnings)
	}
}

func TestReserveReleasesOnIdleAndEstimatedBeforeUntil(t *testing.T) {
	for _, state := range []WindowState{WindowIdle, WindowEstimated} {
		cfg := probeConfig()
		until := testNow.Add(80 * time.Hour)
		ss := snaps(cfg, liveWindows(), livePriorities())
		ss[4].Schedulable = false
		ss[4].Win7d = Window{State: state, Reset: testNow.Add(-2 * time.Hour)}
		if state == WindowEstimated {
			ss[4].Win7d.Reset = testNow.Add(150 * time.Hour)
		}
		prev := State{DisabledUntil: map[int64]int64{1: until.Unix()}, FableUntil: until.Unix(), RoutingOwned: true}
		ss[4].WinFable = Window{State: state, Reset: ss[4].Win7d.Reset}
		live := GroupRouting{Routing: map[string][]int64{"claude-fable-*": {9, 11, 4, 8}}, Enabled: true}
		d, err := Evaluate(cfg, ss, live, prev, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if !releasesReserve(&d, 1) || len(d.State.DisabledUntil) != 0 {
			t.Fatalf("%s before until must release the 7d reserve: %+v state=%+v", state, d.Actions, d.State)
		}
		if d.State.FableUntil != 0 {
			t.Fatalf("%s before until must release the Fable reserve: %+v", state, d.State)
		}
		r := routingAction(d)
		if r == nil || r.Enabled || len(r.Routing) != 0 {
			t.Fatalf("%s: expected routing restored, got %+v", state, d.Actions)
		}
	}
}

func TestProbeEstimateRetiredByNewerSamples(t *testing.T) {
	cfg := probeConfig()
	probedAt := testNow.Add(-72 * time.Hour)
	// A later ended window: idle with a different reset -> record retired, probed again now.
	ss := snaps(cfg, idleWindows(), livePriorities())
	ss[3].Win7d = Window{State: WindowIdle, Reset: testNow.Add(-2 * time.Hour), SampledAt: testNow.Add(-time.Hour)}
	d, _ := Evaluate(cfg, ss, GroupRouting{}, probedState(probedAt), testNow)
	for _, a := range d.Accounts {
		if a.ID == 8 && a.Win7d.State != WindowIdle {
			t.Fatalf("newer ended window must beat the estimate: %+v", a)
		}
	}
	if got := probeActions(d); len(got) != 1 {
		t.Fatalf("newer ended window must be probed again: %+v", d.Actions)
	}
	if _, ok := d.State.Probes[8]; ok {
		t.Fatalf("stale record must be retired: %+v", d.State.Probes)
	}
	// Same ended window but sampled after the probe: also retired.
	ss = snaps(cfg, idleWindows(), livePriorities())
	ss[3].Win7d.SampledAt = probedAt.Add(time.Hour)
	d, _ = Evaluate(cfg, ss, GroupRouting{}, probedState(probedAt), testNow)
	if _, ok := d.State.Probes[8]; ok || len(probeActions(d)) != 1 {
		t.Fatalf("post-probe sample of the same ended window must retire the record: %+v %+v", d.State.Probes, d.Actions)
	}
	// A live window retires the record too.
	ss = snaps(cfg, idleWindows(), livePriorities())
	ss[3].Win7d = known(96, testNow.Add(100*time.Hour))
	d, _ = Evaluate(cfg, ss, GroupRouting{}, probedState(probedAt), testNow)
	if _, ok := d.State.Probes[8]; ok {
		t.Fatalf("known sample must retire the record: %+v", d.State.Probes)
	}
	// After that, a wiped (unknown) window must not resurrect the estimate.
	ss = snaps(cfg, idleWindows(), livePriorities())
	ss[3].Win7d = Window{State: WindowUnknown}
	d, _ = Evaluate(cfg, ss, GroupRouting{}, d.State, testNow)
	for _, a := range d.Accounts {
		if a.ID == 8 && a.Win7d.State != WindowUnknown {
			t.Fatalf("retired record must not estimate: %+v", a)
		}
	}
	// An unknown window right after the probe (5h wipe) keeps the estimate.
	ss = snaps(cfg, idleWindows(), livePriorities())
	ss[3].Win7d = Window{State: WindowUnknown}
	d, _ = Evaluate(cfg, ss, GroupRouting{}, probedState(testNow.Add(-time.Hour)), testNow)
	for _, a := range d.Accounts {
		if a.ID == 8 && a.Win7d.State != WindowEstimated {
			t.Fatalf("unknown after probe must keep the estimate: %+v", a)
		}
	}
}

func TestProbeEstimateIsOffWithFeatureOrExemption(t *testing.T) {
	st := probedState(testNow.Add(-time.Hour))
	cfg := probeConfig()
	cfg.RestartIdleWindows = false
	d, _ := Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, st, testNow)
	for _, a := range d.Accounts {
		if a.ID == 8 && a.Win7d.State != WindowIdle {
			t.Fatalf("feature off must drop the estimate: %+v", a)
		}
	}
	if len(probeActions(d)) != 0 {
		t.Fatalf("feature off must not probe: %+v", d.Actions)
	}
	cfg = probeConfig()
	cfg.Accounts[3].ProbeExempt = true
	d, _ = Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, st, testNow)
	for _, a := range d.Accounts {
		if a.ID == 8 && a.Win7d.State != WindowIdle {
			t.Fatalf("probe_exempt must drop the estimate: %+v", a)
		}
	}
	if len(probeActions(d)) != 0 {
		t.Fatalf("probe_exempt must not probe: %+v", d.Actions)
	}
}

func TestPlanProbesHonoursCooldownAndReserveRelease(t *testing.T) {
	cfg := probeConfig()
	recent := State{Probes: map[int64]ProbeRecord{8: {At: testNow.Add(-5 * time.Hour).Unix(), OK: false}}}
	d, _ := Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, recent, testNow)
	if got := probeActions(d); len(got) != 0 {
		t.Fatalf("failed probe 5h ago must wait for the 6h cooldown: %+v", got)
	}
	old := State{Probes: map[int64]ProbeRecord{8: {At: testNow.Add(-7 * time.Hour).Unix(), OK: false}}}
	d, _ = Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, old, testNow)
	if got := probeActions(d); len(got) != 1 {
		t.Fatalf("failed probe 7h ago must be retried: %+v", d.Actions)
	}
	if _, ok := d.State.Probes[8]; !ok {
		t.Fatal("evaluate must carry probe records forward")
	}
	// A reserve released in this very run counts as schedulable.
	sixty := 60.0
	cfg.Accounts[3].CeilingPercent = &sixty
	cfg.Accounts[3].EnforceCeiling = true
	ss := snaps(cfg, idleWindows(), livePriorities())
	ss[3].Schedulable = false
	disabled := State{DisabledUntil: map[int64]int64{8: testNow.Add(-24 * time.Hour).Unix()}}
	d, _ = Evaluate(cfg, ss, GroupRouting{}, disabled, testNow)
	if !releasesReserve(&d, 8) {
		t.Fatalf("expected reserve release: %+v", d.Actions)
	}
	if got := probeActions(d); len(got) != 1 {
		t.Fatalf("released account must be probed: %+v", d.Actions)
	}
}

func TestProbeEstimateOverlaysIdleWindowUntilSampled(t *testing.T) {
	cfg := probeConfig()
	probedAt := testNow.Add(-30 * time.Minute)
	st := probedState(probedAt)
	d, _ := Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, st, testNow)
	var mine AccountDecision
	for _, a := range d.Accounts {
		if a.ID == 8 {
			mine = a
		}
	}
	if mine.Win7d.State != WindowEstimated || mine.Urgent || mine.Win7d.UsedPercent != 0 {
		t.Fatalf("expected estimated, not urgent: %+v", mine)
	}
	if h := mine.Win7d.HoursToReset(testNow); h < 167 || h > 169 {
		t.Fatalf("estimated reset should be about a week out: %v", h)
	}
	if got := probeActions(d); len(got) != 0 {
		t.Fatalf("estimated window must not be probed again: %+v", got)
	}
	// Once the estimate is inside the lookahead it is urgent like a known window.
	later := probedAt.Add(weekly - 48*time.Hour)
	d, _ = Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, st, later)
	for _, a := range d.Accounts {
		if a.ID == 8 && (!a.Urgent || a.Win7d.State != WindowEstimated || a.Headroom != 100) {
			t.Fatalf("estimated window inside lookahead must be urgent: %+v", a)
		}
	}
	// Real sample after the probe wins over the estimate.
	ss := snaps(cfg, idleWindows(), livePriorities())
	ss[3].Win7d = known(3, probedAt.Add(weekly))
	d, _ = Evaluate(cfg, ss, GroupRouting{}, st, testNow)
	for _, a := range d.Accounts {
		if a.ID == 8 && (a.Win7d.State != WindowKnown || a.Win7d.UsedPercent != 3) {
			t.Fatalf("sampled window must replace estimate: %+v", a)
		}
	}
	// Expired estimate with no sample: idle again and probed again.
	d, _ = Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, st, probedAt.Add(weekly+2*time.Hour))
	if got := probeActions(d); len(got) != 1 {
		t.Fatalf("expired estimate must lead to a new probe: %+v", d.Actions)
	}
	// A failed probe never yields an estimate.
	failed := State{Probes: map[int64]ProbeRecord{8: {At: probedAt.Unix(), OK: false}}}
	d, _ = Evaluate(cfg, snaps(cfg, idleWindows(), livePriorities()), GroupRouting{}, failed, testNow)
	for _, a := range d.Accounts {
		if a.ID == 8 && a.Win7d.State != WindowIdle {
			t.Fatalf("failed probe must not estimate: %+v", a)
		}
	}
}

func TestEstimatedWindowCanBeDrained(t *testing.T) {
	cfg := drainConfig()
	cfg.RestartIdleWindows = true
	probedAt := testNow.Add(-weekly + 20*time.Hour)
	ended := probedAt.Add(-24 * time.Hour)
	wins := liveWindows()
	wins[8] = [2]Window{{State: WindowIdle, Reset: ended, SampledAt: ended.Add(-time.Hour)}, {State: WindowIdle, Reset: ended}}
	st := State{Probes: map[int64]ProbeRecord{8: {At: probedAt.Unix(), OK: true, BaselineReset: ended.Unix(), BaselineSampledAt: ended.Add(-time.Hour).Unix()}}}
	d, _ := Evaluate(cfg, snaps(cfg, wins, livePriorities()), GroupRouting{}, st, testNow)
	if d.DrainAccount != 8 {
		t.Fatalf("estimated window inside drain_hours must be drained: drain=%d actions=%+v", d.DrainAccount, d.Actions)
	}
}
