package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type recorded struct {
	Method, Path, Key string
	Body              map[string]any
}

func newServer(t *testing.T, rec *[]recorded) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		*rec = append(*rec, recorded{r.Method, r.URL.RequestURI(), r.Header.Get("x-api-key"), body})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/admin/accounts":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"items":[{"id":1,"name":"Anthropic","platform":"anthropic","type":"setup-token","priority":100,"status":"active","schedulable":true,"extra":{"passive_usage_7d_utilization":0.32,"passive_usage_7d_reset":1788768000,"passive_usage_7d_oi_utilization":0.44,"passive_usage_7d_oi_reset":1788768000,"passive_usage_sampled_at":"2026-09-03T08:41:42Z"},"account_groups":[{"account_id":1,"group_id":12,"priority":3}]},{"id":9,"name":"relay","platform":"anthropic","type":"apikey","priority":1,"status":"active","schedulable":true,"extra":{},"account_groups":[{"account_id":9,"group_id":12,"priority":2}]}],"total":2,"page":1,"page_size":200,"pages":1}}`))
		case r.Method == "GET" && r.URL.Path == "/api/v1/admin/groups/12":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"id":12,"name":"xb","model_routing":{"claude-fable-*":[9]},"model_routing_enabled":true}}`))
		case r.URL.Path == "/api/v1/admin/accounts/1" || r.URL.Path == "/api/v1/admin/accounts/1/schedulable" || r.URL.Path == "/api/v1/admin/groups/12":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"message":"nope"}`))
		}
	}))
}

func TestClientReadsAccountsAndGroup(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	c := NewAdminClient(srv.URL, "admin-test")
	accts, err := c.ListGroupAccounts(context.Background(), 12)
	if err != nil || len(accts) != 2 || accts[0].Priority != 100 || accts[0].Extra["passive_usage_7d_utilization"] != 0.32 {
		t.Fatalf("accounts=%+v err=%v", accts, err)
	}
	if rec[0].Path != "/api/v1/admin/accounts?group=12&page=1&page_size=200" || rec[0].Key != "admin-test" {
		t.Fatalf("request=%+v", rec[0])
	}
	g, err := c.GetGroup(context.Background(), 12)
	if err != nil || !g.ModelRoutingEnabled || g.ModelRouting["claude-fable-*"][0] != 9 {
		t.Fatalf("group=%+v err=%v", g, err)
	}
}

func TestClientWritesOnlyNamedFields(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	c := NewAdminClient(srv.URL, "admin-test")
	ctx := context.Background()
	if err := c.SetPriority(ctx, 1, 5); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSchedulable(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRouting(ctx, 12, map[string][]int64{}, false); err != nil {
		t.Fatal(err)
	}
	if rec[0].Method != "PUT" || rec[0].Path != "/api/v1/admin/accounts/1" || len(rec[0].Body) != 1 || rec[0].Body["priority"] != float64(5) {
		t.Fatalf("priority request=%+v", rec[0])
	}
	if rec[1].Method != "POST" || rec[1].Path != "/api/v1/admin/accounts/1/schedulable" || rec[1].Body["schedulable"] != false {
		t.Fatalf("schedulable request=%+v", rec[1])
	}
	if rec[2].Method != "PUT" || rec[2].Path != "/api/v1/admin/groups/12" || len(rec[2].Body) != 2 || rec[2].Body["model_routing_enabled"] != false {
		t.Fatalf("routing request=%+v", rec[2])
	}
	if _, ok := rec[2].Body["model_routing"].(map[string]any); !ok {
		t.Fatalf("model_routing must be an object, got %+v", rec[2].Body)
	}
}

func TestClientSurfacesAPIErrors(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	c := NewAdminClient(srv.URL, "admin-test")
	if err := c.SetPriority(context.Background(), 2, 1); err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestSnapshotsFromAPI(t *testing.T) {
	cfg := testConfig()
	now := time.Date(2026, 9, 4, 0, 22, 0, 0, time.UTC)
	accounts := []APIAccount{{ID: 1, Priority: 100, Status: "active", Schedulable: true, Extra: map[string]any{"passive_usage_7d_utilization": 0.32, "passive_usage_7d_reset": float64(1788768000), "passive_usage_7d_oi_utilization": 0.44, "passive_usage_7d_oi_reset": float64(1788768000)}}}
	accounts[0].AccountGroups = append(accounts[0].AccountGroups, struct {
		GroupID  int64 `json:"group_id"`
		Priority int   `json:"priority"`
	}{GroupID: 12, Priority: 3})
	ss := SnapshotsFromAPI(cfg, accounts, now)
	if len(ss) != 1 || ss[0].Policy.ID != 1 || !ss[0].InGroup || ss[0].Win7d.UsedPercent != 32 || ss[0].WinFable.UsedPercent != 44 || ss[0].Priority != 100 {
		t.Fatalf("snapshots=%+v", ss)
	}
	accounts = append(accounts, APIAccount{ID: 42, Priority: 1})
	if got := SnapshotsFromAPI(cfg, accounts, now); len(got) != 1 {
		t.Fatalf("non-policy account must be ignored: %+v", got)
	}
}
