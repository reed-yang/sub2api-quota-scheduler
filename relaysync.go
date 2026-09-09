package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// A relay account is an API key pointing at another gateway, so sub2api never
// sees the subscription window behind it and the scheduler can only rank the
// account by base order. When that upstream is itself a sub2api instance the
// operator can administer, its own accounts carry the passive window sub2api
// sampled from Anthropic. relay-sync reads that window and writes it onto the
// local relay account, which lets the scheduler rank the relay on the same
// terms as a direct subscription.
//
// The sync is one-way and additive: it reads the upstream through its admin
// API and merges three keys into the local account's extra. It never writes to
// the upstream, and on any failure it writes nothing at all, so a stalled sync
// leaves the sample to age out and the scheduler falls back to the base order
// (see AccountPolicy.WindowMaxAgeHours).

// RelaySyncConfig is the on-disk configuration of the relay-sync command.
type RelaySyncConfig struct {
	// UpstreamBaseURL is the other sub2api instance, e.g. https://gateway.example.com.
	UpstreamBaseURL string `json:"upstream_base_url"`
	// The upstream admin credentials are read from these environment
	// variables so they can live in a root-only EnvironmentFile.
	UpstreamEmailEnv    string `json:"upstream_email_env"`
	UpstreamPasswordEnv string `json:"upstream_password_env"`
	// UpstreamPlatform and UpstreamGroupID narrow the upstream accounts to the
	// ones that can serve this key. GroupID 0 means every group.
	UpstreamPlatform string `json:"upstream_platform"`
	UpstreamGroupID  int64  `json:"upstream_group_id"`
	// MaxSampleAgeHours rejects an upstream sample older than this, so the sync
	// never copies a window that stopped moving. Defaults to 6.
	MaxSampleAgeHours float64 `json:"max_sample_age_hours"`
	// The local instance and the relay account the window is written to.
	LocalBaseURL     string `json:"local_base_url"`
	LocalAdminKeyEnv string `json:"local_admin_key_env"`
	TargetAccountID  int64  `json:"target_account_id"`
	// SchedulerConfigPath is checked before every run so the sync refuses to
	// write a window the scheduler is not configured to read. Empty disables
	// the check, for a deployment without the scheduler beside it.
	SchedulerConfigPath *string `json:"scheduler_config_path,omitempty"`
}

// DefaultSchedulerConfigPath is where the scheduler unit reads its config.
const DefaultSchedulerConfigPath = "/etc/sub2api-quota-scheduler/config.json"

// SchedulerConfig resolves the path to check, defaulting when the key is absent.
func (c *RelaySyncConfig) SchedulerConfig() string {
	if c.SchedulerConfigPath == nil {
		return DefaultSchedulerConfigPath
	}
	return *c.SchedulerConfigPath
}

// LoadRelaySyncConfig reads and validates the relay-sync configuration.
func LoadRelaySyncConfig(path string) (*RelaySyncConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &RelaySyncConfig{}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.UpstreamPlatform == "" {
		cfg.UpstreamPlatform = "anthropic"
	}
	if cfg.MaxSampleAgeHours == 0 {
		cfg.MaxSampleAgeHours = 6
	}
	if cfg.LocalBaseURL == "" {
		cfg.LocalBaseURL = "http://127.0.0.1:8080"
	}
	if cfg.LocalAdminKeyEnv == "" {
		cfg.LocalAdminKeyEnv = "SUB2API_QUOTA_SCHEDULER_ADMIN_KEY"
	}
	switch {
	case cfg.UpstreamBaseURL == "":
		return nil, errors.New("upstream_base_url is required")
	case cfg.UpstreamEmailEnv == "" || cfg.UpstreamPasswordEnv == "":
		return nil, errors.New("upstream_email_env and upstream_password_env are required")
	case cfg.TargetAccountID <= 0:
		return nil, errors.New("target_account_id must be positive")
	case cfg.MaxSampleAgeHours < 0:
		return nil, errors.New("max_sample_age_hours must not be negative")
	}
	if err := requireSecureURL(cfg.UpstreamBaseURL); err != nil {
		return nil, err
	}
	return cfg, nil
}

// requireSecureURL rejects a plaintext upstream: the admin password and the
// bearer token minted from it cross that link on every run. Loopback is allowed
// so tests and a same-host upstream still work.
func requireSecureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("upstream_base_url: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("upstream_base_url must use https, got %q", raw)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// verifySchedulerTarget refuses to run when the scheduler would ignore what
// this sync writes. Each of these three settings silently discards the window:
// a non-subscription kind is never ranked by deadline, a missing age bound lets
// a stalled sample rank forever, and a probeable account gets a real message
// sent through the relay.
func verifySchedulerTarget(path string, id int64) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return fmt.Errorf("read scheduler config %s: %w", path, err)
	}
	for _, a := range cfg.Accounts {
		if a.ID != id {
			continue
		}
		switch {
		case a.Kind != "subscription":
			return fmt.Errorf("scheduler config %s lists account %d as kind %q, and only kind \"subscription\" is ranked by its window", path, id, a.Kind)
		case a.WindowMaxAgeHours <= 0:
			return fmt.Errorf("scheduler config %s sets no window_max_age_hours on account %d, so a stalled sync would never age out", path, id)
		case !a.ProbeExempt:
			return fmt.Errorf("scheduler config %s does not set probe_exempt on account %d, whose window this sync owns", path, id)
		}
		return nil
	}
	return fmt.Errorf("scheduler config %s has no account %d", path, id)
}

// UpstreamWindow is the 7-day window copied onto the relay account, together
// with the upstream account count it was chosen from.
type UpstreamWindow struct {
	Utilization float64
	Reset       int64
	SampledAt   string
	Eligible    int
}

// selectUpstreamWindow picks the window to copy: among the upstream accounts
// that can serve this key and carry a fresh 7d sample, the one with the most
// quota left, then the earliest reset.
//
// Most-headroom is the honest choice for a relay, because the upstream decides
// per request which of its accounts answers, and this one bounds what the relay
// can still deliver. With more than one eligible account the number therefore
// describes the best case, not the sum, which is why Eligible is reported.
func selectUpstreamWindow(accounts []APIAccount, cfg *RelaySyncConfig, now time.Time) (UpstreamWindow, error) {
	maxAge := time.Duration(cfg.MaxSampleAgeHours * float64(time.Hour))
	best := UpstreamWindow{Utilization: -1}
	considered := 0
	for _, a := range accounts {
		if a.Platform != cfg.UpstreamPlatform || a.Status != "active" || !a.Schedulable {
			continue
		}
		if cfg.UpstreamGroupID != 0 && !inGroup(a, cfg.UpstreamGroupID) {
			continue
		}
		w := NormalizeWindow(a.Extra["passive_usage_7d_utilization"], a.Extra["passive_usage_7d_reset"], a.Extra["passive_usage_sampled_at"], now)
		if w.State != WindowKnown {
			continue
		}
		// A sample dated in the future never ages out, so treat anything past
		// ordinary clock drift as unusable rather than permanently fresh.
		if age := now.Sub(w.SampledAt); w.SampledAt.IsZero() || age > maxAge || age < -clockSkewMargin {
			continue
		}
		considered++
		util := w.UsedPercent / 100
		if best.Utilization < 0 || util < best.Utilization || (util == best.Utilization && w.Reset.Unix() < best.Reset) {
			sampled, _ := a.Extra["passive_usage_sampled_at"].(string)
			best = UpstreamWindow{Utilization: util, Reset: w.Reset.Unix(), SampledAt: sampled}
		}
	}
	if considered == 0 {
		return UpstreamWindow{}, fmt.Errorf("%w: no upstream %s account is active, schedulable, in group %d and sampled within %.1fh", errNoFreshUpstreamWindow, cfg.UpstreamPlatform, cfg.UpstreamGroupID, cfg.MaxSampleAgeHours)
	}
	best.Eligible = considered
	return best, nil
}

// errNoFreshUpstreamWindow marks the one failure that is a normal state rather
// than a fault: the upstream is simply idle, so there is nothing to copy. The
// command reports it and exits 0, leaving the previous sample to age out.
var errNoFreshUpstreamWindow = errors.New("no fresh upstream window")

func inGroup(a APIAccount, id int64) bool {
	for _, g := range a.AccountGroups {
		if g.GroupID == id {
			return true
		}
	}
	return false
}

// UpstreamClient reads another sub2api instance's admin API with a user login,
// caching the access token between runs because it is valid for hours.
type UpstreamClient struct {
	BaseURL   string
	HTTP      *http.Client
	CachePath string
	token     string
	expires   time.Time
}

// NewUpstreamClient builds a client that caches its token at cachePath.
func NewUpstreamClient(baseURL, cachePath string) *UpstreamClient {
	return &UpstreamClient{BaseURL: baseURL, CachePath: cachePath, HTTP: &http.Client{
		Timeout: 30 * time.Second,
		// Following a redirect would replay the login body, password included,
		// to whatever host the response names.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type cachedToken struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// tokenRenewMargin renews before expiry so a run never starts with a token
// that dies mid-request.
const tokenRenewMargin = 5 * time.Minute

// Token returns a usable access token, logging in only when the cached one is
// missing or about to expire.
func (u *UpstreamClient) Token(ctx context.Context, email, password string, now time.Time) (string, error) {
	if u.token != "" && now.Add(tokenRenewMargin).Before(u.expires) {
		return u.token, nil
	}
	if raw, err := os.ReadFile(u.CachePath); err == nil {
		var c cachedToken
		if json.Unmarshal(raw, &c) == nil && c.AccessToken != "" && now.Add(tokenRenewMargin).Before(c.ExpiresAt) {
			u.token, u.expires = c.AccessToken, c.ExpiresAt
			return u.token, nil
		}
	}
	tok, ttl, err := u.login(ctx, email, password)
	if err != nil {
		return "", err
	}
	u.token, u.expires = tok, now.Add(ttl)
	u.saveToken()
	return u.token, nil
}

func (u *UpstreamClient) saveToken() {
	if u.CachePath == "" {
		return
	}
	raw, err := json.Marshal(cachedToken{AccessToken: u.token, ExpiresAt: u.expires})
	if err != nil {
		return
	}
	tmp := u.CachePath + ".new"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	if os.Rename(tmp, u.CachePath) != nil {
		_ = os.Remove(tmp)
	}
}

func (u *UpstreamClient) login(ctx context.Context, email, password string) (string, time.Duration, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Requires2FA bool   `json:"requires_2fa"`
	}
	if err := u.do(ctx, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": email, "password": password}, &out); err != nil {
		return "", 0, fmt.Errorf("upstream login: %w", err)
	}
	if out.Requires2FA {
		return "", 0, errors.New("upstream login requires a 2FA code, which this command cannot supply")
	}
	if out.AccessToken == "" {
		return "", 0, errors.New("upstream login returned no access token")
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return out.AccessToken, ttl, nil
}

// httpStatusError carries the status so a caller can tell a revoked token from
// an upstream that is merely down.
type httpStatusError struct {
	Method, Path string
	Status       int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s: http %d", e.Method, e.Path, e.Status)
}

func isAuthStatus(err error) bool {
	var e *httpStatusError
	return errors.As(err, &e) && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

// forgetToken drops the cached token so the next call logs in again.
func (u *UpstreamClient) forgetToken() {
	u.token, u.expires = "", time.Time{}
	if u.CachePath != "" {
		_ = os.Remove(u.CachePath)
	}
}

// Accounts lists the upstream accounts, logging in again if the token was
// rejected. A cached token outlives its server-side session whenever the
// upstream restarts its signing key or the password changes, and without this
// the sync would stay wedged until the cached expiry, up to a day away.
func (u *UpstreamClient) Accounts(ctx context.Context, email, password string, now time.Time) ([]APIAccount, error) {
	token, err := u.Token(ctx, email, password, now)
	if err != nil {
		return nil, err
	}
	accounts, err := u.ListAccounts(ctx, token)
	if err == nil || !isAuthStatus(err) {
		return accounts, err
	}
	u.forgetToken()
	if token, err = u.Token(ctx, email, password, now); err != nil {
		return nil, err
	}
	return u.ListAccounts(ctx, token)
}

// ListAccounts returns every account the upstream admin API reports.
func (u *UpstreamClient) ListAccounts(ctx context.Context, token string) ([]APIAccount, error) {
	var page struct {
		Items []APIAccount `json:"items"`
		Total int64        `json:"total"`
	}
	if err := u.do(ctx, http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=500", token, nil, &page); err != nil {
		return nil, fmt.Errorf("upstream list accounts: %w", err)
	}
	if page.Total > int64(len(page.Items)) {
		// Silently ranking a truncated list could pick a window that is not the
		// best one available, or miss the only fresh one.
		return nil, fmt.Errorf("upstream reports %d accounts but returned %d; raise the page size", page.Total, len(page.Items))
	}
	return page.Items, nil
}

func (u *UpstreamClient) do(ctx context.Context, method, path, token string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body can echo the credentials that were rejected, so report the
		// status only.
		return &httpStatusError{Method: method, Path: path, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return json.Unmarshal(raw, out)
}

// MergeAccountExtra merges keys into one account's extra, leaving every other
// key in place. bulk-update is the only admin route that merges rather than
// replaces, so it is the only safe way to touch extra on an account whose
// other keys must survive.
func (c *AdminClient) MergeAccountExtra(ctx context.Context, id int64, extra map[string]any) error {
	body := map[string]any{"account_ids": []int64{id}, "extra": extra}
	return c.do(ctx, http.MethodPost, "/api/v1/admin/accounts/bulk-update", body, nil)
}

// RelaySyncResult is the one-line summary a successful run prints.
type RelaySyncResult struct {
	Time              string  `json:"time"`
	TargetAccountID   int64   `json:"target_account_id"`
	Utilization       float64 `json:"utilization"`
	Reset             string  `json:"reset"`
	UpstreamSampledAt string  `json:"upstream_sampled_at"`
	EligibleAccounts  int     `json:"eligible_upstream_accounts"`
}

// relaySync performs one sync. Every failure path returns before the write, so
// a broken sync leaves the previous sample to age out rather than replacing it
// with something wrong.
func relaySync(ctx context.Context, cfg *RelaySyncConfig, up *UpstreamClient, local *AdminClient, now time.Time) (RelaySyncResult, error) {
	email, password := os.Getenv(cfg.UpstreamEmailEnv), os.Getenv(cfg.UpstreamPasswordEnv)
	if email == "" || password == "" {
		return RelaySyncResult{}, fmt.Errorf("environment variables %s and %s must both be set", cfg.UpstreamEmailEnv, cfg.UpstreamPasswordEnv)
	}
	accounts, err := up.Accounts(ctx, email, password, now)
	if err != nil {
		return RelaySyncResult{}, err
	}
	w, err := selectUpstreamWindow(accounts, cfg, now)
	if err != nil {
		return RelaySyncResult{}, err
	}
	extra := map[string]any{
		"passive_usage_7d_utilization": w.Utilization,
		"passive_usage_7d_reset":       w.Reset,
		// The upstream's own sample time travels with the window, so the
		// scheduler's staleness bound covers a stalled upstream as well as a
		// stalled sync.
		"passive_usage_sampled_at": w.SampledAt,
	}
	if err := local.MergeAccountExtra(ctx, cfg.TargetAccountID, extra); err != nil {
		return RelaySyncResult{}, fmt.Errorf("write relay window: %w", err)
	}
	// bulk-update reports success for ids it never matched, so without a read
	// back a wrong or deleted target_account_id looks like a working sync
	// forever while the scheduler ranks the relay on nothing.
	got, err := local.GetAccount(ctx, cfg.TargetAccountID)
	if err != nil {
		return RelaySyncResult{}, fmt.Errorf("verify relay window: %w", err)
	}
	if stored, _ := got.Extra["passive_usage_sampled_at"].(string); stored != w.SampledAt {
		return RelaySyncResult{}, fmt.Errorf("account %d did not take the window: passive_usage_sampled_at is %q after writing %q", cfg.TargetAccountID, stored, w.SampledAt)
	}
	return RelaySyncResult{
		Time:              now.UTC().Format(time.RFC3339),
		TargetAccountID:   cfg.TargetAccountID,
		Utilization:       w.Utilization,
		Reset:             time.Unix(w.Reset, 0).UTC().Format(time.RFC3339),
		UpstreamSampledAt: w.SampledAt,
		EligibleAccounts:  w.Eligible,
	}, nil
}

// relaySyncCommand wires the relay-sync subcommand.
func relaySyncCommand(args []string) error {
	fs := flag.NewFlagSet("relay-sync", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/sub2api-quota-scheduler/relay-sync.json", "relay-sync config file")
	stateDir := fs.String("state-dir", os.Getenv("STATE_DIRECTORY"), "state directory (defaults to $STATE_DIRECTORY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadRelaySyncConfig(*configPath)
	if err != nil {
		return err
	}
	if path := cfg.SchedulerConfig(); path != "" {
		if err := verifySchedulerTarget(path, cfg.TargetAccountID); err != nil {
			return err
		}
	}
	key := os.Getenv(cfg.LocalAdminKeyEnv)
	if key == "" {
		return fmt.Errorf("environment variable %s is empty", cfg.LocalAdminKeyEnv)
	}
	if *stateDir == "" {
		return errors.New("state directory required (--state-dir or $STATE_DIRECTORY)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := relaySync(ctx, cfg, NewUpstreamClient(cfg.UpstreamBaseURL, filepath.Join(*stateDir, "relay-sync-token.json")), NewAdminClient(cfg.LocalBaseURL, key), time.Now())
	if errors.Is(err, errNoFreshUpstreamWindow) {
		// An idle upstream is the expected steady state between its own
		// requests, not a fault worth failing the unit every quarter hour.
		fmt.Fprintln(os.Stderr, err)
		return nil
	}
	if err != nil {
		return err
	}
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
	return nil
}
