package aiproxy

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// This fails if the agent-budget store stops treating absent rows as four
// unlimited limits, or if it uses rolling rather than calendar windows.
func TestAgentBudgetStatusUsesCalendarWindows(t *testing.T) {
	loc := time.FixedZone("operator", -7*60*60)
	now := time.Date(2026, time.August, 19, 10, 30, 0, 0, loc) // Wednesday
	st := newStore(t)

	status, err := st.AgentBudgetStatus("alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if status.HourUSD != 0 || status.DayUSD != 0 || status.WeekUSD != 0 || status.MonthUSD != 0 || len(status.Exhausted) != 0 {
		t.Fatalf("missing budget status = %+v, want four unlimited limits", status)
	}
	if err := st.SetAgentBudget("alice", AgentBudget{HourUSD: 1, DayUSD: 3, WeekUSD: 4, MonthUSD: 5}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []AIRequest{
		sampleReq("current-hour", "alice", "basic", 1.25, now.Add(-20*time.Minute)),
		sampleReq("previous-hour", "alice", "basic", 2, now.Add(-2*time.Hour)),
		sampleReq("monday", "alice", "basic", 1, time.Date(2026, time.August, 17, 1, 0, 0, 0, loc)),
		sampleReq("previous-week", "alice", "basic", 9, time.Date(2026, time.August, 16, 23, 0, 0, 0, loc)),
		sampleReq("month", "alice", "basic", 0.5, time.Date(2026, time.August, 1, 0, 0, 0, 0, loc)),
		sampleReq("previous-month", "alice", "basic", 20, time.Date(2026, time.July, 31, 23, 59, 0, 0, loc)),
	} {
		if err := st.Insert(row); err != nil {
			t.Fatal(err)
		}
	}

	status, err = st.AgentBudgetStatus("alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if status.HourSpentUSD != 1.25 || status.DaySpentUSD != 3.25 || status.WeekSpentUSD != 4.25 || status.MonthSpentUSD != 13.75 {
		t.Fatalf("calendar spend = %+v; want hour/day/week/month 1.25/3.25/4.25/13.75", status)
	}
	if got, want := status.Exhausted, []string{"hour", "day", "week", "month"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("exhausted = %v, want %v", got, want)
	}
}

func TestSetAgentBudgetRejectsInvalidLimits(t *testing.T) {
	st := newStore(t)
	for _, budget := range []AgentBudget{{HourUSD: -1}, {DayUSD: math.Inf(1)}, {WeekUSD: math.NaN()}} {
		if err := st.SetAgentBudget("alice", budget); err == nil {
			t.Fatalf("SetAgentBudget(%+v) succeeded, want validation error", budget)
		}
	}
}

// This fails if CostSince compares RFC3339Nano text lexically: a fractional
// timestamp immediately after a whole-second calendar boundary sorts before
// the boundary's trailing Z even though it belongs inside the window.
func TestAgentBudgetStatusIncludesFractionalSecondAfterCalendarBoundary(t *testing.T) {
	st := newStore(t)
	now := time.Date(2026, time.July, 6, 10, 0, 1, 0, time.UTC)
	if err := st.SetAgentBudget("alice", AgentBudget{HourUSD: 0.50}); err != nil {
		t.Fatal(err)
	}
	if err := st.Insert(sampleReq("fractional", "alice", "basic", 0.60, time.Date(2026, time.July, 6, 10, 0, 0, 1, time.UTC))); err != nil {
		t.Fatal(err)
	}

	status, err := st.AgentBudgetStatus("alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if status.HourSpentUSD != 0.60 || strings.Join(status.Exhausted, ",") != "hour" {
		t.Fatalf("calendar-boundary status = %+v, want hour spent 0.60 and exhausted hour", status)
	}
}

// This fails if the window sums stop normalizing timestamps with an offset
// other than Z, or pick up another agent's spend.
func TestAgentBudgetStatusNormalizesOffsetTimestamps(t *testing.T) {
	st := newStore(t)
	now := time.Date(2026, time.July, 6, 10, 30, 0, 0, time.UTC)
	for _, row := range []AIRequest{
		{ID: "east", TS: "2026-07-06T12:30:00+03:00", Agent: "alice", CostUSD: 1},   // 09:30Z: day, not hour
		{ID: "west", TS: "2026-07-06T05:10:00.5-05:00", Agent: "alice", CostUSD: 2}, // 10:10:00.5Z: hour
		{ID: "other", TS: "2026-07-06T10:15:00Z", Agent: "bob", CostUSD: 40},
	} {
		if err := st.Insert(row); err != nil {
			t.Fatal(err)
		}
	}
	status, err := st.AgentBudgetStatus("alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if status.HourSpentUSD != 2 || status.DaySpentUSD != 3 || status.WeekSpentUSD != 3 || status.MonthSpentUSD != 3 {
		t.Fatalf("offset spend = %+v, want hour/day/week/month 2/3/3/3", status)
	}
}

// Budget status runs for every agent on each agent-list poll and on every
// proxied request. Scanning each agent's whole request history four times held
// the daemon's single database connection long enough to queue every other
// request behind it, so the window sums must seek a bounded range of one
// covering index.
func TestAgentBudgetSpendSeeksCoveringIndexRange(t *testing.T) {
	st := newStore(t)
	query, args := agentWindowSpendQuery("alice", time.Date(2026, time.July, 6, 10, 30, 0, 0, time.UTC))
	rows, err := st.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "USING COVERING INDEX idx_ai_requests_agent_ts_cost (agent=? AND ts>?)") {
		t.Fatalf("query plan does not seek the covering index by agent and time:\n%s", joined)
	}
}

func TestBudgetStoreCRUD(t *testing.T) {
	s := newStore(t)
	if err := s.SetBudget(Budget{Scope: "agent:alice", LimitUSD: 1.0, PeriodS: 3600, Mode: "block"}); err != nil {
		t.Fatal(err)
	}
	// Upsert.
	if err := s.SetBudget(Budget{Scope: "agent:alice", LimitUSD: 2.0, PeriodS: 3600, Mode: "warn"}); err != nil {
		t.Fatal(err)
	}
	b, ok, _ := s.GetBudget("agent:alice")
	if !ok || b.LimitUSD != 2.0 || b.Mode != "warn" {
		t.Fatalf("get budget = %+v ok=%v", b, ok)
	}
	list, _ := s.ListBudgets()
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if err := s.DeleteBudget("agent:alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetBudget("agent:alice"); ok {
		t.Fatal("delete failed")
	}
}

func TestBudgetCacheCheck(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	st := newStore(t)
	// Spend $0.80 in the last hour for alice.
	st.Insert(sampleReq("r1", "alice", "basic", 0.80, now.Add(-30*time.Minute)))
	st.SetBudget(Budget{Scope: "agent:alice", LimitUSD: 0.50, PeriodS: 3600, Mode: "block"})

	cache := NewBudgetCache(st, func() time.Time { return now })
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}
	d := cache.Check("alice")
	if !d.Over || d.Mode != "block" || d.Scope != "agent:alice" {
		t.Fatalf("decision = %+v", d)
	}
	// bob has no budget -> not over.
	if cache.Check("bob").Over {
		t.Fatal("bob should not be over")
	}
}

// This fails if agent budget enforcement waits for the periodic cache refresh
// after a newly persisted cost exhausts a calendar window.
func TestBudgetCacheCheckUsesLatestAgentSpendWithoutRefresh(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	st := newStore(t)
	if err := st.SetAgentBudget("alice", AgentBudget{HourUSD: 0.50}); err != nil {
		t.Fatal(err)
	}
	cache := NewBudgetCache(st, func() time.Time { return now })
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}
	if cache.Check("alice").Over {
		t.Fatal("agent should be within budget before any cost is persisted")
	}
	if err := st.Insert(sampleReq("newly-exhausted", "alice", "basic", 0.60, now)); err != nil {
		t.Fatal(err)
	}

	d := cache.Check("alice")
	if !d.Over || d.Mode != "block" || strings.Join(d.Exhausted, ",") != "hour" {
		t.Fatalf("decision after persisted cost = %+v, want an immediate hour block", d)
	}
}

// This fails if an already-cached agent block survives a later update that
// raises the limit (including zero, which means unlimited).
func TestBudgetCacheCheckUsesLatestAgentLimitsWithoutRefresh(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	st := newStore(t)
	if err := st.Insert(sampleReq("already-exhausted", "alice", "basic", 0.60, now)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAgentBudget("alice", AgentBudget{HourUSD: 0.50}); err != nil {
		t.Fatal(err)
	}
	cache := NewBudgetCache(st, func() time.Time { return now })
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}
	if !cache.Check("alice").Over {
		t.Fatal("agent should start over budget")
	}
	if err := st.SetAgentBudget("alice", AgentBudget{}); err != nil {
		t.Fatal(err)
	}
	if d := cache.Check("alice"); d.Over {
		t.Fatalf("decision after removing limits = %+v, want within budget", d)
	}
}

func TestBudgetBlockMiddleware(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	st := newStore(t)
	st.Insert(sampleReq("r1", "alice", "basic", 0.80, now.Add(-time.Minute)))
	st.SetBudget(Budget{Scope: "agent:alice", LimitUSD: 0.50, PeriodS: 3600, Mode: "block"})
	cache := NewBudgetCache(st, func() time.Time { return now })
	cache.Refresh()

	p := testProxy(t)
	p.cfg.Budget = cache
	p.cfg.Store = st
	p.rebuild() // pick up the budget middleware now that a cache is present
	forwarded := false
	p.forward = func(ex *Exchange) error { forwarded = true; ex.W.WriteHeader(200); return nil }

	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-9", ImageName: "basic", ImageTag: "latest"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)

	if rr.Code != 429 {
		t.Fatalf("blocked request status = %d, want 429", rr.Code)
	}
	if forwarded {
		t.Fatal("blocked request must not reach the upstream")
	}
}

// This fails if the proxy stops applying a configured calendar agent limit
// before reaching the upstream provider.
func TestAgentCalendarBudgetBlockMiddleware(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 30, 0, 0, time.UTC)
	st := newStore(t)
	if err := st.Insert(sampleReq("r1", "alice", "basic", 0.80, now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAgentBudget("alice", AgentBudget{HourUSD: 0.50}); err != nil {
		t.Fatal(err)
	}
	cache := NewBudgetCache(st, func() time.Time { return now })
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}
	status, err := st.AgentBudgetStatus("alice", now)
	if err != nil || strings.Join(status.Exhausted, ",") != "hour" {
		t.Fatalf("stored agent budget status = %+v err=%v, want exhausted hour", status, err)
	}
	p := testProxy(t)
	p.cfg.Budget = cache
	p.cfg.Store = st
	p.rebuild()
	forwarded := false
	p.forward = func(ex *Exchange) error { forwarded = true; ex.W.WriteHeader(http.StatusOK); return nil }
	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-9", ImageName: "basic", ImageTag: "latest"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests || forwarded || !strings.Contains(rr.Body.String(), "hour") {
		t.Fatalf("agent calendar budget response code=%d forwarded=%v body=%s", rr.Code, forwarded, rr.Body.String())
	}
}

func TestBudgetWarnMiddlewareProceeds(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	st := newStore(t)
	st.Insert(sampleReq("r1", "alice", "basic", 0.80, now.Add(-time.Minute)))
	st.SetBudget(Budget{Scope: "agent:alice", LimitUSD: 0.50, PeriodS: 3600, Mode: "warn"})
	cache := NewBudgetCache(st, func() time.Time { return now })
	cache.Refresh()

	p := testProxy(t)
	p.cfg.Budget = cache
	p.cfg.Store = st
	warned := 0
	p.cfg.Warn = func(agent string, d Decision) { warned++ }
	p.rebuild()
	forwarded := false
	p.forward = func(ex *Exchange) error { forwarded = true; ex.W.WriteHeader(200); return nil }

	tok, _ := p.Mint(Attribution{Agent: "alice", Iteration: "alice-9", ImageName: "basic", ImageTag: "latest"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/_tariboy/"+tok+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	req.Header.Set("x-api-key", "real-provider-key")
	p.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("warned request status = %d, want 200", rr.Code)
	}
	if !forwarded {
		t.Fatal("warn must proceed to the upstream")
	}
	if warned != 1 {
		t.Fatalf("warn callback called %d times, want 1", warned)
	}
}

// TestBudgetCacheRace exercises the refresher goroutine writing the cache while
// the request path reads it, so `go test -race` proves the RWMutex covers both.
func TestBudgetCacheRace(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	st := newStore(t)
	st.Insert(sampleReq("r1", "alice", "basic", 0.80, now.Add(-time.Minute)))
	st.SetBudget(Budget{Scope: "agent:alice", LimitUSD: 0.50, PeriodS: 3600, Mode: "block"})
	cache := NewBudgetCache(st, func() time.Time { return now })
	cache.Refresh()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = cache.Refresh()
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = cache.Check("alice")
		}
		close(stop)
	}()
	wg.Wait()
}
