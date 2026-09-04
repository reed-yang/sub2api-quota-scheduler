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
	if c != 95 || f != 95 {
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
