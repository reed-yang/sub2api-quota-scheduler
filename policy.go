package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// AccountSnapshot is the live view of one policy account.
type AccountSnapshot struct {
	Policy      AccountPolicy
	BaseIndex   int
	Priority    int
	Schedulable bool
	Status      string
	InGroup     bool
	Win7d       Window
	WinFable    Window
}

// GroupRouting is the live model routing of the policy group.
type GroupRouting struct {
	Routing map[string][]int64
	Enabled bool
}

// ProbeRecord remembers the last probe attempt on an account. At drives the
// cooldown; OK marks a delivered probe whose window may be estimated. The
// baseline fields capture the passive window seen at probe time so that any
// newer sample invalidates the estimate.
type ProbeRecord struct {
	At                int64 `json:"at"`
	OK                bool  `json:"ok"`
	BaselineReset     int64 `json:"baseline_reset,omitempty"`
	BaselineSampledAt int64 `json:"baseline_sampled_at,omitempty"`
}

// maxProbesPerRun bounds how many probes one run may send, so a cluster of
// idle accounts cannot stretch a run past the systemd timer interval.
const maxProbesPerRun = 3

// Final-window promotion is independent of pressure and minimum headroom.
const finalWindowHours = 5

// State is persisted between runs.
type State struct {
	LastOrder     []int64         `json:"last_order"`
	DisabledUntil map[int64]int64 `json:"disabled_until"`
	FableUntil    int64           `json:"fable_until"`
	// RoutingOwned records that the group's model routing was written by the
	// scheduler, so it may be rewritten or cleared.
	RoutingOwned   bool                  `json:"routing_owned"`
	DrainAccountID int64                 `json:"drain_account_id"`
	DrainUntil     int64                 `json:"drain_until"`
	Probes         map[int64]ProbeRecord `json:"probes"`
	LastRun        string                `json:"last_run"`
}

func (s State) clone() State {
	out := State{LastOrder: append([]int64(nil), s.LastOrder...), DisabledUntil: map[int64]int64{}, FableUntil: s.FableUntil, RoutingOwned: s.RoutingOwned, DrainAccountID: s.DrainAccountID, DrainUntil: s.DrainUntil, Probes: map[int64]ProbeRecord{}, LastRun: s.LastRun}
	for k, v := range s.DisabledUntil {
		out.DisabledUntil[k] = v
	}
	for k, v := range s.Probes {
		out.Probes[k] = v
	}
	return out
}

// Action is one write the scheduler wants to perform.
type Action struct {
	Type      string             `json:"type"`
	AccountID int64              `json:"account_id,omitempty"`
	From      int                `json:"from,omitempty"`
	To        int                `json:"to,omitempty"`
	Value     bool               `json:"value"`
	Routing   map[string][]int64 `json:"routing,omitempty"`
	Enabled   bool               `json:"enabled"`
	// Model is the probe model; Result is filled in by apply mode.
	Model  string `json:"model,omitempty"`
	Result string `json:"result,omitempty"`
	Reason string `json:"reason"`
}

// AccountDecision is the per-account explanation in the decision log.
// HoursToReset is null for windows without a deadline (idle, unknown).
type AccountDecision struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	Schedulable     bool     `json:"schedulable"`
	Win7d           Window   `json:"win_7d"`
	WinFable        Window   `json:"win_fable"`
	Ceiling         float64  `json:"ceiling_percent"`
	FableCeiling    float64  `json:"fable_ceiling_percent"`
	Headroom        float64  `json:"headroom_percent"`
	HoursToReset    *float64 `json:"hours_to_reset"`
	Pressure        float64  `json:"pressure"`
	Urgent          bool     `json:"urgent"`
	FinalWindow     bool     `json:"final_window"`
	CurrentPriority int      `json:"current_priority"`
	TargetPriority  int      `json:"target_priority"`
}

// Decision is the full output of one evaluation.
type Decision struct {
	Time         time.Time         `json:"time"`
	Mode         string            `json:"mode"`
	GroupID      int64             `json:"group_id"`
	Accounts     []AccountDecision `json:"accounts"`
	TargetOrder  []int64           `json:"target_order"`
	DrainAccount int64             `json:"drain_account_id"`
	Actions      []Action          `json:"actions"`
	Warnings     []string          `json:"warnings"`
	State        State             `json:"-"`
}

type candidate struct {
	snap     AccountSnapshot
	decision *AccountDecision
}

// Evaluate computes the target order and reserve actions without side effects.
func Evaluate(cfg *Config, snaps []AccountSnapshot, group GroupRouting, prev State, now time.Time) (Decision, error) {
	byID := map[int64]AccountSnapshot{}
	for _, s := range snaps {
		byID[s.Policy.ID] = s
	}
	for _, a := range cfg.Accounts {
		s, ok := byID[a.ID]
		if !ok {
			return Decision{}, fmt.Errorf("policy account %d (%s) missing from live accounts", a.ID, a.Name)
		}
		if !s.InGroup {
			return Decision{}, fmt.Errorf("policy account %d (%s) is not bound to group %d", a.ID, a.Name, cfg.GroupID)
		}
	}

	d := Decision{Time: now.UTC(), Mode: cfg.Mode, GroupID: cfg.GroupID, State: prev.clone(), Warnings: []string{}, Actions: []Action{}}
	applyProbeEstimates(cfg, byID, &d, now)
	// Pre-size Accounts so the per-account pointers below stay valid across appends.
	d.Accounts = make([]AccountDecision, 0, len(cfg.Accounts))
	var finalWindow, urgent, normal []candidate
	for i, a := range cfg.Accounts {
		s := byID[a.ID]
		s.BaseIndex = i
		ceiling, fable := cfg.Ceiling(a)
		ad := AccountDecision{ID: a.ID, Name: a.Name, Kind: a.Kind, Schedulable: s.Schedulable, Win7d: s.Win7d, WinFable: s.WinFable, Ceiling: ceiling, FableCeiling: fable, CurrentPriority: s.Priority}
		d.Accounts = append(d.Accounts, ad)
		c := candidate{snap: s, decision: &d.Accounts[len(d.Accounts)-1]}
		if a.Kind == "subscription" && s.Win7d.State != WindowUnknown {
			c.decision.Headroom = ceiling - s.Win7d.UsedPercent
		}
		if a.Kind == "subscription" && s.Win7d.HasDeadline() {
			hours := s.Win7d.HoursToReset(now)
			headroom := c.decision.Headroom
			c.decision.HoursToReset = &hours
			if hours > 0 && hours <= finalWindowHours && headroom > 0 && s.Schedulable && s.Status == "active" {
				c.decision.Pressure = headroom / math.Max(hours, 1)
				c.decision.Urgent = true
				c.decision.FinalWindow = true
				finalWindow = append(finalWindow, c)
				continue
			}
			if hours > 0 && hours <= cfg.LookaheadHours && headroom >= cfg.MinUrgentHeadroomPercent {
				c.decision.Pressure = headroom / math.Max(hours, 1)
				c.decision.Urgent = true
				urgent = append(urgent, c)
				continue
			}
		}
		normal = append(normal, c)
	}

	sort.SliceStable(finalWindow, func(i, j int) bool {
		a, b := finalWindow[i], finalWindow[j]
		if !a.snap.Win7d.Reset.Equal(b.snap.Win7d.Reset) {
			return a.snap.Win7d.Reset.Before(b.snap.Win7d.Reset)
		}
		if a.decision.Headroom != b.decision.Headroom {
			return a.decision.Headroom > b.decision.Headroom
		}
		return a.snap.BaseIndex < b.snap.BaseIndex
	})
	sortUrgent(urgent, prev.LastOrder, cfg.HysteresisRatio)
	ordered := append(append(finalWindow, urgent...), normal...)
	for pos, c := range ordered {
		target := pos + 1
		c.decision.TargetPriority = target
		d.TargetOrder = append(d.TargetOrder, c.snap.Policy.ID)
		if c.snap.Priority != target {
			reason := "base order"
			if c.decision.FinalWindow {
				reason = fmt.Sprintf("final 5h: headroom %.1f%%, reset in %.1fh", c.decision.Headroom, *c.decision.HoursToReset)
			} else if c.decision.Urgent {
				reason = fmt.Sprintf("urgent: headroom %.1f%% over %.1fh", c.decision.Headroom, *c.decision.HoursToReset)
			}
			d.Actions = append(d.Actions, Action{Type: "set_priority", AccountID: c.snap.Policy.ID, From: c.snap.Priority, To: target, Reason: reason})
		}
	}
	d.State.LastOrder = append([]int64(nil), d.TargetOrder...)

	desired := map[string][]int64{}
	for _, a := range cfg.Accounts {
		if !a.EnforceCeiling {
			continue
		}
		s := byID[a.ID]
		ceiling, fable := cfg.Ceiling(a)
		evaluateReserve(cfg, &d, s, ceiling, now)
		if list := fableReserveRouting(cfg, &d, s, fable, now); list != nil {
			desired[cfg.FableModelPattern] = list
		}
	}
	if target := selectDrainTarget(cfg, &d, byID, now); target != nil {
		desired[cfg.DrainModelPattern] = []int64{target.Policy.ID}
		d.DrainAccount = target.Policy.ID
		d.State.DrainAccountID = target.Policy.ID
		d.State.DrainUntil = target.Win7d.Reset.Unix()
	} else {
		d.State.DrainAccountID, d.State.DrainUntil = 0, 0
	}
	reconcileRouting(cfg, &d, group, desired)
	planProbes(cfg, &d, byID, now)
	return d, nil
}

// applyProbeEstimates overlays the window a delivered probe is expected to
// have started, for as long as sub2api still shows the same ended window (or
// none at all) that was seen at probe time. Any newer sample, whether a live
// window or a later ended one, retires the probe record so sampled data always
// wins; an expired estimate leaves the account idle again. Nothing is
// overlaid when the feature is off or the account is exempt, so turning the
// feature off is a complete rollback.
func applyProbeEstimates(cfg *Config, byID map[int64]AccountSnapshot, d *Decision, now time.Time) {
	if !cfg.RestartIdleWindows {
		return
	}
	for _, a := range cfg.Accounts {
		rec, ok := d.State.Probes[a.ID]
		if a.Kind != "subscription" || a.ProbeExempt || !ok || !rec.OK {
			continue
		}
		s := byID[a.ID]
		w := s.Win7d
		probedAt := time.Unix(rec.At, 0).UTC()
		newer := w.State == WindowKnown || w.SampledAt.After(probedAt) || (w.State == WindowIdle && w.Reset.Unix() != rec.BaselineReset)
		if newer {
			delete(d.State.Probes, a.ID)
			continue
		}
		if w.State != WindowIdle && w.State != WindowUnknown {
			continue
		}
		if est, ok := EstimatedWindow(probedAt, now); ok {
			s.Win7d = est
			byID[a.ID] = s
		}
	}
}

// planProbes appends a probe action for each subscription without a window
// deadline (idle, or never sampled) whose window should be started, at most
// maxProbesPerRun per run. Probes come after every other action and never
// affect the current run's ordering.
func planProbes(cfg *Config, d *Decision, byID map[int64]AccountSnapshot, now time.Time) {
	if !cfg.RestartIdleWindows {
		return
	}
	cooldown := time.Duration(cfg.ProbeCooldownHours * float64(time.Hour))
	planned := 0
	for _, a := range cfg.Accounts {
		if planned >= maxProbesPerRun {
			d.Warnings = append(d.Warnings, fmt.Sprintf("probe cap of %d per run reached; remaining idle accounts wait for the next run", maxProbesPerRun))
			return
		}
		if a.Kind != "subscription" || a.ProbeExempt {
			continue
		}
		s := byID[a.ID]
		if s.Win7d.HasDeadline() || s.Status != "active" {
			continue
		}
		if !s.Schedulable && !releasesReserve(d, a.ID) {
			continue
		}
		if rec, ok := d.State.Probes[a.ID]; ok && now.Sub(time.Unix(rec.At, 0)) < cooldown {
			continue
		}
		reason := "no 7d sample on record; sending one message so Anthropic starts the window"
		if s.Win7d.State == WindowIdle {
			reason = fmt.Sprintf("7d window ended %s with no request since; sending one message so Anthropic starts the next window", s.Win7d.Reset.Format(time.RFC3339))
		}
		d.Actions = append(d.Actions, Action{Type: "probe", AccountID: a.ID, Model: cfg.ProbeModel, Reason: reason})
		planned++
	}
}

// releasesReserve reports whether this decision re-enables the account.
func releasesReserve(d *Decision, id int64) bool {
	for _, a := range d.Actions {
		if a.Type == "set_schedulable" && a.AccountID == id && a.Value {
			return true
		}
	}
	return false
}

// selectDrainTarget returns the non-exempt subscription whose 7-day window
// resets soonest within cfg.DrainHours and still has usable headroom, or nil.
func selectDrainTarget(cfg *Config, d *Decision, byID map[int64]AccountSnapshot, now time.Time) *AccountSnapshot {
	if cfg.DrainHours <= 0 {
		return nil
	}
	var best *AccountSnapshot
	for _, a := range cfg.Accounts {
		if a.Kind != "subscription" || a.EnforceCeiling || a.DrainExempt {
			continue
		}
		s := byID[a.ID]
		if !s.Schedulable || s.Status != "active" || !s.Win7d.HasDeadline() {
			continue
		}
		ceiling, _ := cfg.Ceiling(a)
		hours := s.Win7d.HoursToReset(now)
		if s.Win7d.UsedPercent >= ceiling || hours <= 0 || hours > cfg.DrainHours {
			continue
		}
		if best == nil || s.Win7d.Reset.Before(best.Win7d.Reset) || (s.Win7d.Reset.Equal(best.Win7d.Reset) && s.Win7d.UsedPercent < best.Win7d.UsedPercent) {
			c := s
			best = &c
		}
	}
	return best
}

// reconcileRouting writes the desired group routing when the scheduler owns
// the group's routing (or the routing is empty), and clears it when nothing is
// desired anymore. Routing written by someone else is never touched.
func reconcileRouting(cfg *Config, d *Decision, group GroupRouting, desired map[string][]int64) {
	liveEmpty := len(group.Routing) == 0
	if len(desired) == 0 {
		if !liveEmpty && d.State.RoutingOwned {
			d.Actions = append(d.Actions, Action{Type: "set_routing", Routing: map[string][]int64{}, Enabled: false, Reason: "no reserve or drain active; restoring empty routing"})
		} else if !liveEmpty {
			d.Warnings = append(d.Warnings, fmt.Sprintf("group %d has routing the scheduler did not write; leaving it untouched", cfg.GroupID))
		}
		d.State.RoutingOwned = false
		return
	}
	if !liveEmpty && !d.State.RoutingOwned && !routingEqual(group.Routing, desired) {
		d.Warnings = append(d.Warnings, fmt.Sprintf("group %d routing is owned by someone else; not applying reserve/drain routing", cfg.GroupID))
		return
	}
	if !routingEqual(group.Routing, desired) || !group.Enabled {
		reason := "reserve/drain routing"
		if d.DrainAccount != 0 {
			reason = fmt.Sprintf("drain account %d until %s", d.DrainAccount, time.Unix(d.State.DrainUntil, 0).UTC().Format(time.RFC3339))
		}
		d.Actions = append(d.Actions, Action{Type: "set_routing", Routing: desired, Enabled: true, Reason: reason})
	}
	d.State.RoutingOwned = true
}

// sortUrgent orders by pressure desc, then earlier reset, then base index, but
// keeps the previous relative order of two accounts whose pressures are within
// the hysteresis ratio.
func sortUrgent(urgent []candidate, prevOrder []int64, ratio float64) {
	prevIndex := map[int64]int{}
	for i, id := range prevOrder {
		prevIndex[id] = i
	}
	before := func(a, b candidate) bool {
		pa, pb := a.decision.Pressure, b.decision.Pressure
		ia, okA := prevIndex[a.snap.Policy.ID]
		ib, okB := prevIndex[b.snap.Policy.ID]
		hi, lo := math.Max(pa, pb), math.Min(pa, pb)
		if okA && okB && hi <= lo*(1+ratio) {
			return ia < ib
		}
		if pa != pb {
			return pa > pb
		}
		if !a.snap.Win7d.Reset.Equal(b.snap.Win7d.Reset) {
			return a.snap.Win7d.Reset.Before(b.snap.Win7d.Reset)
		}
		return a.snap.BaseIndex < b.snap.BaseIndex
	}
	sort.SliceStable(urgent, func(i, j int) bool { return before(urgent[i], urgent[j]) })
}

func evaluateReserve(cfg *Config, d *Decision, s AccountSnapshot, ceiling float64, now time.Time) {
	id := s.Policy.ID
	w := s.Win7d
	if w.State == WindowKnown && w.UsedPercent >= ceiling {
		if s.Schedulable {
			d.Actions = append(d.Actions, Action{Type: "set_schedulable", AccountID: id, Value: false, Reason: fmt.Sprintf("7d %.1f%% >= reserve ceiling %.0f%%, until %s", w.UsedPercent, ceiling, w.Reset.Format(time.RFC3339))})
		}
		d.State.DisabledUntil[id] = w.Reset.Unix()
		return
	}
	until, disabledByUs := d.State.DisabledUntil[id]
	if !disabledByUs {
		return
	}
	if now.Unix() >= until || w.State == WindowIdle || w.State == WindowEstimated {
		if !s.Schedulable {
			d.Actions = append(d.Actions, Action{Type: "set_schedulable", AccountID: id, Value: true, Reason: "7d window reset; releasing scheduler-imposed reserve"})
		}
		delete(d.State.DisabledUntil, id)
	}
}

// fableReserveRouting returns the Fable routing list (every policy account
// except the reserved one) while the reserved account's Fable window is at or
// above its ceiling, and nil once that window resets.
func fableReserveRouting(cfg *Config, d *Decision, s AccountSnapshot, fableCeiling float64, now time.Time) []int64 {
	others := []int64{}
	for _, a := range cfg.Accounts {
		if a.ID != s.Policy.ID {
			others = append(others, a.ID)
		}
	}
	w := s.WinFable
	if w.State == WindowKnown && w.UsedPercent >= fableCeiling {
		d.State.FableUntil = w.Reset.Unix()
		return others
	}
	if d.State.FableUntil != 0 && (now.Unix() >= d.State.FableUntil || w.State == WindowIdle || w.State == WindowEstimated) {
		d.State.FableUntil = 0
	}
	if d.State.FableUntil != 0 {
		return others
	}
	return nil
}

func routingEqual(a, b map[string][]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || len(va) != len(vb) {
			return false
		}
		sa := append([]int64(nil), va...)
		sb := append([]int64(nil), vb...)
		sort.Slice(sa, func(i, j int) bool { return sa[i] < sa[j] })
		sort.Slice(sb, func(i, j int) bool { return sb[i] < sb[j] })
		for i := range sa {
			if sa[i] != sb[i] {
				return false
			}
		}
	}
	return true
}
