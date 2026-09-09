package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var syncNow = time.Date(2026, 9, 9, 21, 0, 0, 0, time.UTC)

func syncConfig() *RelaySyncConfig {
	return &RelaySyncConfig{
		UpstreamBaseURL: "http://upstream.invalid", UpstreamEmailEnv: "U_MAIL", UpstreamPasswordEnv: "U_PASS",
		UpstreamPlatform: "anthropic", UpstreamGroupID: 1, MaxSampleAgeHours: 6,
		LocalBaseURL: "http://127.0.0.1:8080", LocalAdminKeyEnv: "LOCAL_KEY", TargetAccountID: 9,
	}
}

func upstreamAccount(id int64, platform, status string, sched bool, group int64, util float64, reset time.Time, sampled time.Time) APIAccount {
	a := APIAccount{ID: id, Platform: platform, Status: status, Schedulable: sched, Extra: map[string]any{}}
	if !reset.IsZero() {
		a.Extra["passive_usage_7d_utilization"] = util
		a.Extra["passive_usage_7d_reset"] = float64(reset.Unix())
		a.Extra["passive_usage_sampled_at"] = sampled.UTC().Format(time.RFC3339)
	}
	a.AccountGroups = append(a.AccountGroups, struct {
		GroupID  int64 `json:"group_id"`
		Priority int   `json:"priority"`
	}{GroupID: group})
	return a
}

func TestSelectUpstreamWindowPrefersMostHeadroom(t *testing.T) {
	cfg := syncConfig()
	accounts := []APIAccount{
		upstreamAccount(1, "anthropic", "active", true, 1, 0.74, syncNow.Add(15*time.Hour), syncNow.Add(-10*time.Minute)),
		upstreamAccount(2, "anthropic", "active", true, 1, 0.20, syncNow.Add(80*time.Hour), syncNow.Add(-time.Minute)),
	}
	w, err := selectUpstreamWindow(accounts, cfg, syncNow)
	if err != nil {
		t.Fatal(err)
	}
	if w.Utilization != 0.20 || w.Eligible != 2 || w.Reset != syncNow.Add(80*time.Hour).Unix() {
		t.Fatalf("window %+v, want the 20%% account and both counted", w)
	}
}

func TestSelectUpstreamWindowSkipsUnusableAccounts(t *testing.T) {
	cfg := syncConfig()
	for _, tc := range []struct {
		name string
		a    APIAccount
	}{
		{"other platform", upstreamAccount(1, "openai", "active", true, 1, 0.1, syncNow.Add(20*time.Hour), syncNow)},
		{"error status", upstreamAccount(2, "anthropic", "error", true, 1, 0.1, syncNow.Add(20*time.Hour), syncNow)},
		{"unschedulable", upstreamAccount(3, "anthropic", "active", false, 1, 0.1, syncNow.Add(20*time.Hour), syncNow)},
		{"other group", upstreamAccount(4, "anthropic", "active", true, 7, 0.1, syncNow.Add(20*time.Hour), syncNow)},
		{"no window", upstreamAccount(5, "anthropic", "active", true, 1, 0, time.Time{}, time.Time{})},
		{"reset passed", upstreamAccount(6, "anthropic", "active", true, 1, 0.1, syncNow.Add(-time.Hour), syncNow)},
		{"sample too old", upstreamAccount(7, "anthropic", "active", true, 1, 0.1, syncNow.Add(20*time.Hour), syncNow.Add(-7*time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := selectUpstreamWindow([]APIAccount{tc.a}, cfg, syncNow); err == nil {
				t.Fatal("expected no usable upstream window")
			}
		})
	}
}

func TestLoadRelaySyncConfigDefaultsAndValidation(t *testing.T) {
	p := writeTemp(t, `{"upstream_base_url":"https://up.example","upstream_email_env":"E","upstream_password_env":"P","target_account_id":9}`)
	cfg, err := LoadRelaySyncConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamPlatform != "anthropic" || cfg.MaxSampleAgeHours != 6 || cfg.LocalBaseURL != "http://127.0.0.1:8080" || cfg.LocalAdminKeyEnv != "SUB2API_QUOTA_SCHEDULER_ADMIN_KEY" {
		t.Fatalf("defaults %+v", cfg)
	}
	for _, bad := range []string{
		`{"upstream_email_env":"E","upstream_password_env":"P","target_account_id":9}`,
		`{"upstream_base_url":"https://up.example","upstream_password_env":"P","target_account_id":9}`,
		`{"upstream_base_url":"https://up.example","upstream_email_env":"E","upstream_password_env":"P"}`,
		`{"upstream_base_url":"https://up.example","upstream_email_env":"E","upstream_password_env":"P","target_account_id":9,"max_sample_age_hours":-1}`,
	} {
		if _, err := LoadRelaySyncConfig(writeTemp(t, bad)); err == nil {
			t.Fatalf("expected rejection for %s", bad)
		}
	}
}

// newSyncServers stands in for both instances: the upstream admin API and the
// local one that receives the merged extra.
func newSyncServers(t *testing.T, upstreamAccounts []APIAccount) (*httptest.Server, *httptest.Server, *int32, *map[string]any) {
	t.Helper()
	var logins int32
	written := map[string]any{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/auth/login":
			atomic.AddInt32(&logins, 1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["email"] == "" || body["password"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"tok","refresh_token":"r","expires_in":3600}}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/admin/accounts"):
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"items": upstreamAccounts, "total": len(upstreamAccounts)}})
			_, _ = w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "local-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/v1/admin/accounts/bulk-update":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			written = body
			_, _ = w.Write([]byte(`{"code":0,"data":{"updated":1}}`))
		case "/api/v1/admin/accounts/9":
			// The read back sees whatever bulk-update stored.
			extra, _ := written["extra"].(map[string]any)
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"id": 9, "extra": extra}})
			_, _ = w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(local.Close)
	return up, local, &logins, &written
}

func TestRelaySyncWritesWindowAndReusesToken(t *testing.T) {
	accounts := []APIAccount{upstreamAccount(1, "anthropic", "active", true, 1, 0.74, syncNow.Add(15*time.Hour), syncNow.Add(-10*time.Minute))}
	up, local, logins, written := newSyncServers(t, accounts)
	t.Setenv("U_MAIL", "someone@example.com")
	t.Setenv("U_PASS", "secret")
	cfg := syncConfig()
	cfg.UpstreamBaseURL, cfg.LocalBaseURL = up.URL, local.URL
	cache := filepath.Join(t.TempDir(), "token.json")

	res, err := relaySync(context.Background(), cfg, NewUpstreamClient(up.URL, cache), NewAdminClient(local.URL, "local-key"), syncNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Utilization != 0.74 || res.EligibleAccounts != 1 || res.Reset != syncNow.Add(15*time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("result %+v", res)
	}
	extra, _ := (*written)["extra"].(map[string]any)
	if extra["passive_usage_7d_utilization"] != 0.74 || extra["passive_usage_sampled_at"] != syncNow.Add(-10*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("written extra %+v, want the upstream sample time to travel with the window", extra)
	}
	if ids, _ := (*written)["account_ids"].([]any); len(ids) != 1 || ids[0].(float64) != 9 {
		t.Fatalf("account_ids %+v", (*written)["account_ids"])
	}

	// A second run inside the token's lifetime must not log in again.
	if _, err := relaySync(context.Background(), cfg, NewUpstreamClient(up.URL, cache), NewAdminClient(local.URL, "local-key"), syncNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(logins); got != 1 {
		t.Fatalf("logins %d, want the cached token reused", got)
	}
	if raw, err := os.ReadFile(cache); err != nil || !strings.Contains(string(raw), "access_token") {
		t.Fatalf("token cache %q err %v", raw, err)
	}
}

func TestRelaySyncWritesNothingWhenUpstreamHasNoUsableWindow(t *testing.T) {
	stale := []APIAccount{upstreamAccount(1, "anthropic", "active", true, 1, 0.74, syncNow.Add(15*time.Hour), syncNow.Add(-9*time.Hour))}
	up, local, _, written := newSyncServers(t, stale)
	t.Setenv("U_MAIL", "someone@example.com")
	t.Setenv("U_PASS", "secret")
	cfg := syncConfig()
	cfg.UpstreamBaseURL, cfg.LocalBaseURL = up.URL, local.URL
	_, err := relaySync(context.Background(), cfg, NewUpstreamClient(up.URL, filepath.Join(t.TempDir(), "t.json")), NewAdminClient(local.URL, "local-key"), syncNow)
	if !errors.Is(err, errNoFreshUpstreamWindow) {
		t.Fatalf("err %v, want errNoFreshUpstreamWindow so the timer does not fail on an idle upstream", err)
	}
	if len(*written) != 0 {
		t.Fatalf("a failed run must not write: %+v", *written)
	}
}

func TestRelaySyncRequiresCredentials(t *testing.T) {
	t.Setenv("U_MAIL", "")
	t.Setenv("U_PASS", "")
	_, err := relaySync(context.Background(), syncConfig(), NewUpstreamClient("http://upstream.invalid", ""), NewAdminClient("http://127.0.0.1:1", "k"), syncNow)
	if err == nil || !strings.Contains(err.Error(), "U_MAIL") {
		t.Fatalf("err %v, want a clear message naming the environment variables", err)
	}
}

// A revoked token must not wedge the sync until the cached expiry: the run has
// to notice the 401 and log in again.
func TestRelaySyncRetriesOnceAfterTokenRevoked(t *testing.T) {
	accounts := []APIAccount{upstreamAccount(1, "anthropic", "active", true, 1, 0.5, syncNow.Add(20*time.Hour), syncNow.Add(-time.Minute))}
	var logins int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			// Only the token minted by the second login is honoured below.
			n := atomic.AddInt32(&logins, 1)
			fmt.Fprintf(w, `{"code":0,"data":{"access_token":"tok%d","expires_in":3600}}`, n)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"items": accounts, "total": 1}})
		_, _ = w.Write(raw)
	}))
	defer up.Close()
	_, local, _, _ := newSyncServers(t, accounts)
	t.Setenv("U_MAIL", "someone@example.com")
	t.Setenv("U_PASS", "secret")
	cfg := syncConfig()
	cfg.UpstreamBaseURL, cfg.LocalBaseURL = up.URL, local.URL
	cache := filepath.Join(t.TempDir(), "token.json")

	if _, err := relaySync(context.Background(), cfg, NewUpstreamClient(up.URL, cache), NewAdminClient(local.URL, "local-key"), syncNow); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&logins); got != 2 {
		t.Fatalf("logins %d, want a second login after the 401", got)
	}
	if raw, _ := os.ReadFile(cache); !strings.Contains(string(raw), "tok2") {
		t.Fatalf("token cache %q, want the working token", raw)
	}
}

// bulk-update answers success for account ids it never matched, so the run has
// to confirm the window landed.
func TestRelaySyncFailsWhenTargetDidNotTakeTheWindow(t *testing.T) {
	accounts := []APIAccount{upstreamAccount(1, "anthropic", "active", true, 1, 0.5, syncNow.Add(20*time.Hour), syncNow.Add(-time.Minute))}
	up, _, _, _ := newSyncServers(t, accounts)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/accounts/bulk-update" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"updated":1}}`))
			return
		}
		// The account exists but kept its old extra.
		_, _ = w.Write([]byte(`{"code":0,"data":{"id":9,"extra":{"passive_usage_sampled_at":"2026-01-01T00:00:00Z"}}}`))
	}))
	defer local.Close()
	t.Setenv("U_MAIL", "someone@example.com")
	t.Setenv("U_PASS", "secret")
	cfg := syncConfig()
	cfg.UpstreamBaseURL, cfg.LocalBaseURL = up.URL, local.URL

	_, err := relaySync(context.Background(), cfg, NewUpstreamClient(up.URL, filepath.Join(t.TempDir(), "t.json")), NewAdminClient(local.URL, "local-key"), syncNow)
	if err == nil || !strings.Contains(err.Error(), "did not take the window") {
		t.Fatalf("err %v, want the read back to catch a write that did not land", err)
	}
}

// Ranking a truncated page could miss the freshest window entirely.
func TestListAccountsRejectsTruncatedPage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"id":1}],"total":900}}`))
	}))
	defer up.Close()
	_, err := NewUpstreamClient(up.URL, "").ListAccounts(context.Background(), "tok")
	if err == nil || !strings.Contains(err.Error(), "raise the page size") {
		t.Fatalf("err %v, want a loud failure on truncation", err)
	}
}

func TestRequireSecureURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://gateway.example.com", true},
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"http://gateway.example.com", false},
		{"gateway.example.com", false},
	} {
		if err := requireSecureURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("%s: err %v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

// A sample dated in the future would otherwise never age out.
func TestSelectUpstreamWindowRejectsFutureSample(t *testing.T) {
	accounts := []APIAccount{upstreamAccount(1, "anthropic", "active", true, 1, 0.4, syncNow.Add(20*time.Hour), syncNow.Add(2*time.Hour))}
	if _, err := selectUpstreamWindow(accounts, syncConfig(), syncNow); !errors.Is(err, errNoFreshUpstreamWindow) {
		t.Fatalf("err %v, want a future-dated sample to be unusable", err)
	}
}

// The sync must refuse to write a window the scheduler is configured to ignore.
func TestVerifySchedulerTarget(t *testing.T) {
	write := func(t *testing.T, account string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		body := `{"base_url":"http://127.0.0.1:8080","group_id":1,"accounts":[` + account + `]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name    string
		account string
		want    string
	}{
		{"ready", `{"id":9,"name":"relay","kind":"subscription","probe_exempt":true,"window_max_age_hours":3}`, ""},
		{"still a relay", `{"id":9,"name":"relay","kind":"relay","window_max_age_hours":3}`, "only kind"},
		{"never ages out", `{"id":9,"name":"relay","kind":"subscription","probe_exempt":true}`, "window_max_age_hours"},
		{"probeable", `{"id":9,"name":"relay","kind":"subscription","window_max_age_hours":3}`, "probe_exempt"},
		{"absent", `{"id":4,"name":"other","kind":"subscription"}`, "no account 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifySchedulerTarget(write(t, tc.account), 9)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err %v, want the configuration accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
