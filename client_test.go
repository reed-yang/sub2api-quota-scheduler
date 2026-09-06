package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	Method, Path, Key string
	Body              map[string]any
}

func newServer(t *testing.T, rec *[]recorded) *httptest.Server {
	t.Helper()
	return newServerWithReset(t, rec, 1788768000)
}

// newServerWithReset serves a fake admin API where account 1's passive 7d
// reset is the given unix time, and a test endpoint that streams SSE.
func newServerWithReset(t *testing.T, rec *[]recorded, reset int64) *httptest.Server {
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
			_, _ = fmt.Fprintf(w, `{"code":0,"message":"success","data":{"items":[{"id":1,"name":"Anthropic","platform":"anthropic","type":"setup-token","priority":100,"status":"active","schedulable":true,"extra":{"passive_usage_7d_utilization":0.32,"passive_usage_7d_reset":%d,"passive_usage_7d_oi_utilization":0.44,"passive_usage_7d_oi_reset":%d,"passive_usage_sampled_at":"2026-09-03T08:41:42Z"},"account_groups":[{"account_id":1,"group_id":12,"priority":3}]},{"id":9,"name":"relay","platform":"anthropic","type":"apikey","priority":1,"status":"active","schedulable":true,"extra":{},"account_groups":[{"account_id":9,"group_id":12,"priority":2}]}],"total":2,"page":1,"page_size":200,"pages":1}}`, reset, reset)
		case r.Method == "GET" && r.URL.Path == "/api/v1/admin/groups/12":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"id":12,"name":"xb","model_routing":{"claude-fable-*":[9]},"model_routing_enabled":true}}`))
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/accounts/1/test":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"test_start\",\"model\":\"claude-haiku-4-5-20251001\"}\n\ndata: {\"type\":\"content\",\"text\":\"Hi!\"}\n\ndata: {\"type\":\"test_complete\",\"success\":true}\n\n"))
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/accounts/2/test":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"test_start\",\"model\":\"claude-haiku-4-5-20251001\"}\n\ndata: {\"type\":\"error\",\"error\":\"API returned 429: rate limited\"}\n\n"))
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/accounts/3/test":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"test_start\"}\n\ndata: {\"type\":\"content\",\"text\":\"partial\"}\n\n"))
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/accounts/4/test":
			// sub2api's EOF path: completion without any content.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"test_start\"}\n\ndata: {\"type\":\"test_complete\",\"success\":true}\n\n"))
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/accounts/6/test":
			// Oversized upstream error body inside one SSE line.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"error\",\"error\":\"API returned 529: %s\"}\n\n", strings.Repeat("x", 2<<20))
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/accounts/8/test":
			// Slow completion: longer than a tiny admin timeout.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"test_start\"}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(80 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"type\":\"content\",\"text\":\"Hi\"}\n\ndata: {\"type\":\"test_complete\",\"success\":true}\n\n"))
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/accounts/5/test":
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"code":500,"message":"boom"}`))
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

func TestClientProbeParsesSSEOutcome(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	c := NewAdminClient(srv.URL, "admin-test")
	ctx := context.Background()
	if err := c.ProbeAccount(ctx, 1, "claude-haiku-4-5-20251001"); err != nil {
		t.Fatalf("successful probe: %v", err)
	}
	if rec[0].Method != "POST" || rec[0].Path != "/api/v1/admin/accounts/1/test" || rec[0].Body["model_id"] != "claude-haiku-4-5-20251001" || len(rec[0].Body) != 1 {
		t.Fatalf("probe request=%+v", rec[0])
	}
	if err := c.ProbeAccount(ctx, 2, ""); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("error event must fail the probe, got %v", err)
	}
	if len(rec[1].Body) != 0 {
		t.Fatalf("empty model must send an empty object, got %+v", rec[1].Body)
	}
	if err := c.ProbeAccount(ctx, 3, ""); err == nil || !strings.Contains(err.Error(), "without completion") {
		t.Fatalf("truncated stream must fail, got %v", err)
	}
	if err := c.ProbeAccount(ctx, 5, ""); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("http error must fail, got %v", err)
	}
	if err := c.ProbeAccount(ctx, 7, ""); err == nil {
		t.Fatal("404 must fail")
	}
	if err := c.ProbeAccount(ctx, 4, ""); err == nil || !strings.Contains(err.Error(), "without any content") {
		t.Fatalf("completion without content must fail, got %v", err)
	}
	err := c.ProbeAccount(ctx, 6, "")
	if err == nil || !strings.Contains(err.Error(), "API returned 529") || len(err.Error()) > 3<<20 {
		t.Fatalf("oversized error line must still surface, got len=%d err=%.120v", len(fmt.Sprint(err)), err)
	}
}

func TestReadLineTruncatesWithoutAborting(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("data: " + strings.Repeat("y", 100) + "\nnext\n"))
	line, truncated, err := readLine(r, 20)
	if err != nil || !truncated || line != "data: "+strings.Repeat("y", 14) {
		t.Fatalf("line=%q truncated=%v err=%v", line, truncated, err)
	}
	line, truncated, err = readLine(r, 20)
	if err != nil || truncated || line != "next\n" {
		t.Fatalf("second line=%q truncated=%v err=%v", line, truncated, err)
	}
	if _, _, err = readLine(r, 20); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestClientProbeUsesLongTimeoutClient(t *testing.T) {
	var rec []recorded
	srv := newServer(t, &rec)
	defer srv.Close()
	c := &AdminClient{BaseURL: srv.URL, APIKey: "k", HTTP: &http.Client{Timeout: 10 * time.Millisecond}, ProbeHTTP: &http.Client{Timeout: 5 * time.Second}}
	if err := c.ProbeAccount(context.Background(), 8, ""); err != nil {
		t.Fatalf("probe must use ProbeHTTP, not the short admin client: %v", err)
	}
	c.ProbeHTTP = nil
	if err := c.ProbeAccount(context.Background(), 8, ""); err == nil {
		t.Fatal("without ProbeHTTP the short admin client must time out (fallback exercised)")
	}
}

// newStatefulServer is newServerWithReset plus persisted priorities, so a
// steady-state run produces no writes.
func newStatefulServer(t *testing.T, rec *[]recorded, reset int64) *httptest.Server {
	t.Helper()
	prio := map[int64]int{1: 100, 9: 1}
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		mu.Lock()
		defer mu.Unlock()
		*rec = append(*rec, recorded{r.Method, r.URL.RequestURI(), r.Header.Get("x-api-key"), body})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/admin/accounts":
			_, _ = fmt.Fprintf(w, `{"code":0,"message":"success","data":{"items":[{"id":1,"name":"Anthropic","platform":"anthropic","type":"setup-token","priority":%d,"status":"active","schedulable":true,"extra":{"passive_usage_7d_utilization":0.32,"passive_usage_7d_reset":%d,"passive_usage_7d_oi_utilization":0.44,"passive_usage_7d_oi_reset":%d,"passive_usage_sampled_at":"2026-09-03T08:41:42Z"},"account_groups":[{"account_id":1,"group_id":12,"priority":3}]},{"id":9,"name":"relay","platform":"anthropic","type":"apikey","priority":%d,"status":"active","schedulable":true,"extra":{},"account_groups":[{"account_id":9,"group_id":12,"priority":2}]}],"total":2,"page":1,"page_size":200,"pages":1}}`, prio[1], reset, reset, prio[9])
		case r.Method == "GET" && r.URL.Path == "/api/v1/admin/groups/12":
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"id":12,"name":"xb","model_routing":{},"model_routing_enabled":false}}`))
		case r.Method == "PUT" && (r.URL.Path == "/api/v1/admin/accounts/1" || r.URL.Path == "/api/v1/admin/accounts/9"):
			id := int64(1)
			if r.URL.Path == "/api/v1/admin/accounts/9" {
				id = 9
			}
			prio[id] = int(body["priority"].(float64))
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{}}`))
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/accounts/1/test":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"test_start\"}\n\ndata: {\"type\":\"content\",\"text\":\"Hi!\"}\n\ndata: {\"type\":\"test_complete\",\"success\":true}\n\n"))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"message":"nope"}`))
		}
	}))
}
