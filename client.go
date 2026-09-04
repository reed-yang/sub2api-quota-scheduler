package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// APIAccount is the subset of the admin account DTO the scheduler reads.
type APIAccount struct {
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	Platform      string         `json:"platform"`
	Type          string         `json:"type"`
	Priority      int            `json:"priority"`
	Status        string         `json:"status"`
	Schedulable   bool           `json:"schedulable"`
	Extra         map[string]any `json:"extra"`
	AccountGroups []struct {
		GroupID  int64 `json:"group_id"`
		Priority int   `json:"priority"`
	} `json:"account_groups"`
}

// APIGroup is the subset of the admin group DTO the scheduler reads.
type APIGroup struct {
	ID                  int64              `json:"id"`
	ModelRouting        map[string][]int64 `json:"model_routing"`
	ModelRoutingEnabled bool               `json:"model_routing_enabled"`
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// AdminClient talks to the sub2api admin API with an admin API key.
type AdminClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewAdminClient builds a client with a short timeout suitable for loopback.
func NewAdminClient(baseURL, apiKey string) *AdminClient {
	return &AdminClient{BaseURL: baseURL, APIKey: apiKey, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *AdminClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s %s: status %d, non-JSON body", method, path, resp.StatusCode)
	}
	if resp.StatusCode >= 300 || env.Code != 0 {
		return fmt.Errorf("%s %s: status %d code %d: %s", method, path, resp.StatusCode, env.Code, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// ListGroupAccounts returns every account bound to the group.
func (c *AdminClient) ListGroupAccounts(ctx context.Context, groupID int64) ([]APIAccount, error) {
	var page struct {
		Items []APIAccount `json:"items"`
		Total int64        `json:"total"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/admin/accounts?group=%d&page=1&page_size=200", groupID), nil, &page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

// GetGroup returns the group's routing state.
func (c *AdminClient) GetGroup(ctx context.Context, id int64) (APIGroup, error) {
	var g APIGroup
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/admin/groups/%d", id), nil, &g)
	return g, err
}

// SetPriority updates only the global priority of one account.
func (c *AdminClient) SetPriority(ctx context.Context, id int64, priority int) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/api/v1/admin/accounts/%d", id), map[string]any{"priority": priority}, nil)
}

// SetSchedulable toggles the account's schedulable flag.
func (c *AdminClient) SetSchedulable(ctx context.Context, id int64, v bool) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", id), map[string]any{"schedulable": v}, nil)
}

// SetRouting replaces the group's model routing. An empty map clears it.
func (c *AdminClient) SetRouting(ctx context.Context, groupID int64, routing map[string][]int64, enabled bool) error {
	if routing == nil {
		routing = map[string][]int64{}
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/api/v1/admin/groups/%d", groupID), map[string]any{"model_routing": routing, "model_routing_enabled": enabled}, nil)
}

// SnapshotsFromAPI maps live accounts onto the policy accounts, ignoring others.
func SnapshotsFromAPI(cfg *Config, accounts []APIAccount, now time.Time) []AccountSnapshot {
	byID := map[int64]APIAccount{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	var out []AccountSnapshot
	for i, p := range cfg.Accounts {
		a, ok := byID[p.ID]
		if !ok {
			continue
		}
		s := AccountSnapshot{Policy: p, BaseIndex: i, Priority: a.Priority, Schedulable: a.Schedulable, Status: a.Status}
		for _, g := range a.AccountGroups {
			if g.GroupID == cfg.GroupID {
				s.InGroup = true
			}
		}
		s.Win7d = NormalizeWindow(a.Extra["passive_usage_7d_utilization"], a.Extra["passive_usage_7d_reset"], a.Extra["passive_usage_sampled_at"], now)
		s.WinFable = NormalizeWindow(a.Extra["passive_usage_7d_oi_utilization"], a.Extra["passive_usage_7d_oi_reset"], a.Extra["passive_usage_sampled_at"], now)
		out = append(out, s)
	}
	return out
}
