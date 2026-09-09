package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigDefaultsAndCeilings(t *testing.T) {
	p := writeTemp(t, `{"group_id":12,"accounts":[{"id":9,"name":"relay","kind":"relay"},{"id":1,"name":"my-sub","kind":"subscription","ceiling_percent":60,"fable_ceiling_percent":80,"enforce_ceiling":true}]}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "shadow" || cfg.LookaheadHours != 72 || cfg.DefaultCeilingPercent != 95 || cfg.FableModelPattern != "claude-fable-*" || cfg.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("defaults: %+v", cfg)
	}
	c, f := cfg.Ceiling(cfg.Accounts[0])
	if c != 100 || f != 100 {
		t.Fatalf("relay ceilings %v %v", c, f)
	}
	c, f = cfg.Ceiling(cfg.Accounts[1])
	if c != 60 || f != 80 {
		t.Fatalf("my-sub ceilings %v %v", c, f)
	}
}

func TestLoadConfigRejectsBadInput(t *testing.T) {
	for _, body := range []string{
		`{"group_id":12,"accounts":[]}`,
		`{"group_id":12,"mode":"yolo","accounts":[{"id":1,"name":"a","kind":"subscription"}]}`,
		`{"group_id":12,"accounts":[{"id":1,"name":"a","kind":"subscription"},{"id":1,"name":"b","kind":"relay"}]}`,
		`{"group_id":0,"accounts":[{"id":1,"name":"a","kind":"subscription"}]}`,
		`{"group_id":12,"accounts":[{"id":1,"name":"a","kind":"other"}]}`,
	} {
		if _, err := LoadConfig(writeTemp(t, body)); err == nil {
			t.Fatalf("expected error for %s", body)
		}
	}
}

func TestLoadConfigDrainValidation(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, `{"group_id":12,"drain_hours":24,"accounts":[{"id":1,"name":"a","kind":"subscription"}]}`))
	if err != nil || cfg.DrainHours != 24 || cfg.DrainModelPattern != "claude-*" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if _, err := LoadConfig(writeTemp(t, `{"group_id":12,"drain_hours":-1,"accounts":[{"id":1,"name":"a","kind":"subscription"}]}`)); err == nil {
		t.Fatal("negative drain_hours must fail")
	}
	if _, err := LoadConfig(writeTemp(t, `{"group_id":12,"drain_hours":24,"drain_model_pattern":"claude-fable-*","accounts":[{"id":1,"name":"a","kind":"subscription"}]}`)); err == nil {
		t.Fatal("drain pattern equal to fable pattern must fail")
	}
}

func TestLoadConfigProbeDefaultsAndValidation(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, `{"group_id":12,"accounts":[{"id":1,"name":"a","kind":"subscription","probe_exempt":true},{"id":2,"name":"b","kind":"subscription"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RestartIdleWindows || cfg.ProbeModel != "claude-haiku-4-5-20251001" || cfg.ProbeCooldownHours != 6 {
		t.Fatalf("probe defaults: %+v", cfg)
	}
	if !cfg.Accounts[0].ProbeExempt || cfg.Accounts[1].ProbeExempt {
		t.Fatalf("probe_exempt not honoured: %+v", cfg.Accounts)
	}
	cfg, err = LoadConfig(writeTemp(t, `{"group_id":12,"restart_idle_windows":true,"probe_model":"claude-sonnet-4-5-20250929","probe_cooldown_hours":12,"accounts":[{"id":1,"name":"a","kind":"subscription"}]}`))
	if err != nil || !cfg.RestartIdleWindows || cfg.ProbeModel != "claude-sonnet-4-5-20250929" || cfg.ProbeCooldownHours != 12 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if _, err := LoadConfig(writeTemp(t, `{"group_id":12,"probe_cooldown_hours":-1,"accounts":[{"id":1,"name":"a","kind":"subscription"}]}`)); err == nil {
		t.Fatal("negative probe_cooldown_hours must fail")
	}
}
