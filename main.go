package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("sub2cc-quota-scheduler " + version)
	case "run", "plan":
		if err := command(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sub2cc-quota-scheduler run|plan --config <path> [--state-dir <dir>] [--now RFC3339] | version")
}

func command(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	configPath := fs.String("config", "/etc/sub2cc-scheduler/config.json", "config file")
	stateDir := fs.String("state-dir", os.Getenv("STATE_DIRECTORY"), "state directory (defaults to $STATE_DIRECTORY)")
	nowFlag := fs.String("now", "", "override current time (RFC3339) for planning")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	key := os.Getenv(cfg.AdminKeyEnv)
	if key == "" {
		return fmt.Errorf("environment variable %s is empty", cfg.AdminKeyEnv)
	}
	if *stateDir == "" {
		return errors.New("state directory required (--state-dir or $STATE_DIRECTORY)")
	}
	now := time.Now()
	if *nowFlag != "" {
		if now, err = time.Parse(time.RFC3339, *nowFlag); err != nil {
			return err
		}
	}
	write := name == "run" && cfg.Mode == "apply"
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	d, err := runOnce(ctx, cfg, NewAdminClient(cfg.BaseURL, key), *stateDir, write, now)
	if err != nil {
		return err
	}
	out, _ := json.Marshal(d)
	fmt.Println(string(out))
	return nil
}

// runOnce fetches, evaluates, optionally applies, and persists one decision.
func runOnce(ctx context.Context, cfg *Config, client *AdminClient, stateDir string, write bool, now time.Time) (Decision, error) {
	prev, err := LoadState(stateDir)
	if err != nil {
		return Decision{}, fmt.Errorf("load state: %w", err)
	}
	accounts, err := client.ListGroupAccounts(ctx, cfg.GroupID)
	if err != nil {
		return Decision{}, fmt.Errorf("list accounts: %w", err)
	}
	group, err := client.GetGroup(ctx, cfg.GroupID)
	if err != nil {
		return Decision{}, fmt.Errorf("get group: %w", err)
	}
	d, err := Evaluate(cfg, SnapshotsFromAPI(cfg, accounts, now), GroupRouting{Routing: group.ModelRouting, Enabled: group.ModelRoutingEnabled}, prev, now)
	if err != nil {
		return Decision{}, err
	}
	if write {
		if errs := Apply(ctx, client, d); len(errs) > 0 {
			for _, e := range errs {
				d.Warnings = append(d.Warnings, "apply: "+e.Error())
			}
		}
	} else if len(d.Actions) > 0 {
		d.Warnings = append(d.Warnings, "shadow mode: actions not applied")
	}
	d.State.LastRun = now.UTC().Format(time.RFC3339)
	if err := AppendDecision(stateDir, d); err != nil {
		return d, fmt.Errorf("append decision: %w", err)
	}
	if err := SaveState(stateDir, d.State); err != nil {
		return d, fmt.Errorf("save state: %w", err)
	}
	return d, nil
}

// Apply executes the decision's actions in order and collects errors.
func Apply(ctx context.Context, c *AdminClient, d Decision) []error {
	var errs []error
	for _, a := range d.Actions {
		var err error
		switch a.Type {
		case "set_priority":
			err = c.SetPriority(ctx, a.AccountID, a.To)
		case "set_schedulable":
			err = c.SetSchedulable(ctx, a.AccountID, a.Value)
		case "set_routing":
			err = c.SetRouting(ctx, d.GroupID, a.Routing, a.Enabled)
		default:
			err = fmt.Errorf("unknown action %q", a.Type)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
