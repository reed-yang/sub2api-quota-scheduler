package main

import (
	"context"
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
