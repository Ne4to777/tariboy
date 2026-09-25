package aiproxy

import (
	"database/sql"
	"fmt"
	"math"
	"sync"
	"time"
)

// AgentBudget is the four independently configured USD limits for one agent.
// A zero limit means unlimited for that calendar window.
type AgentBudget struct {
	HourUSD, DayUSD, WeekUSD, MonthUSD float64
}

// AgentBudgetStatus joins one agent's configured limits with its current local
// calendar-window spend. Exhausted is ordered hour, day, week, month.
type AgentBudgetStatus struct {
	AgentBudget
	HourSpentUSD, DaySpentUSD, WeekSpentUSD, MonthSpentUSD float64
	Exhausted                                              []string
}

func validAgentBudget(b AgentBudget) error {
	for _, v := range []float64{b.HourUSD, b.DayUSD, b.WeekUSD, b.MonthUSD} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("agent budget limits must be finite non-negative USD values")
		}
	}
	return nil
}

// SetAgentBudget atomically saves all four limits after full validation.
func (s *Store) SetAgentBudget(agent string, b AgentBudget) error {
	if err := validAgentBudget(b); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO agent_budgets(agent_name,hour_usd,day_usd,week_usd,month_usd)
		VALUES (?,?,?,?,?) ON CONFLICT(agent_name) DO UPDATE SET
		hour_usd=excluded.hour_usd, day_usd=excluded.day_usd,
		week_usd=excluded.week_usd, month_usd=excluded.month_usd`,
		agent, b.HourUSD, b.DayUSD, b.WeekUSD, b.MonthUSD)
	return err
}

func (s *Store) ListAgentBudgetNames() ([]string, error) {
	rows, err := s.db.Query(`SELECT agent_name FROM agent_budgets WHERE hour_usd > 0 OR day_usd > 0 OR week_usd > 0 OR month_usd > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func localCalendarStarts(now time.Time) (hour, day, week, month time.Time) {
	loc := now.Location()
	hour = time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, loc)
	day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	week = day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	month = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	return hour, day, week, month
}

// agentWindowSpendQuery sums one agent's hour, day, week and month spend in a
// single pass. Each window compares offset-aware times at the same millisecond
// precision as CostSince's strftime, but julianday is several times cheaper
// per row; LIMIT -1 keeps SQLite from flattening the subquery so it runs once
// per row, not once per window. The plain ts range only bounds the index seek.
// It starts a day before the earliest window because ts is RFC3339 text whose
// offset may differ from Z by up to 14 hours, so its lexical order is exact
// only across whole days.
func agentWindowSpendQuery(agent string, now time.Time) (string, []any) {
	hour, day, week, month := localCalendarStarts(now)
	earliest := week
	if month.Before(earliest) {
		earliest = month
	}
	const sum = `COALESCE(SUM(CASE WHEN t>=julianday(?) THEN cost_usd END),0)`
	query := `SELECT ` + sum + `,` + sum + `,` + sum + `,` + sum + ` FROM (
		SELECT julianday(ts) AS t, cost_usd FROM ai_requests WHERE agent=? AND ts>=? LIMIT -1)`
	args := []any{}
	for _, start := range []time.Time{hour, day, week, month} {
		args = append(args, start.UTC().Format(time.RFC3339Nano))
	}
	return query, append(args, agent, earliest.Add(-24*time.Hour).UTC().Format(time.RFC3339Nano))
}

// AgentBudgetStatus returns zero limits for a missing row and derives spending
// from immutable request costs for the current local calendar periods.
func (s *Store) AgentBudgetStatus(agent string, now time.Time) (AgentBudgetStatus, error) {
	status := AgentBudgetStatus{}
	err := s.db.QueryRow(`SELECT hour_usd,day_usd,week_usd,month_usd FROM agent_budgets WHERE agent_name=?`, agent).
		Scan(&status.HourUSD, &status.DayUSD, &status.WeekUSD, &status.MonthUSD)
	if err != nil && err != sql.ErrNoRows {
		return AgentBudgetStatus{}, err
	}
	query, args := agentWindowSpendQuery(agent, now)
	if err := s.db.QueryRow(query, args...).Scan(&status.HourSpentUSD, &status.DaySpentUSD,
		&status.WeekSpentUSD, &status.MonthSpentUSD); err != nil {
		return AgentBudgetStatus{}, err
	}
	for _, spent := range []*float64{&status.HourSpentUSD, &status.DaySpentUSD, &status.WeekSpentUSD, &status.MonthSpentUSD} {
		*spent = roundCost(*spent)
	}
	for _, window := range []struct {
		name         string
		limit, spent float64
	}{
		{"hour", status.HourUSD, status.HourSpentUSD}, {"day", status.DayUSD, status.DaySpentUSD},
		{"week", status.WeekUSD, status.WeekSpentUSD}, {"month", status.MonthUSD, status.MonthSpentUSD},
	} {
		if window.limit > 0 && window.spent >= window.limit {
			status.Exhausted = append(status.Exhausted, window.name)
		}
	}
	return status, nil
}

type Budget struct {
	Scope     string // agent:<name> | group:<g> | global
	LimitUSD  float64
	PeriodS   int
	Mode      string // warn | block
	CreatedAt string
}

func (s *Store) SetBudget(b Budget) error {
	if b.Mode == "" {
		b.Mode = "warn"
	}
	if b.PeriodS == 0 {
		b.PeriodS = 86400
	}
	_, err := s.db.Exec(`INSERT INTO budgets(scope, limit_usd, period_s, mode)
		VALUES (?,?,?,?)
		ON CONFLICT(scope) DO UPDATE SET limit_usd=excluded.limit_usd,
			period_s=excluded.period_s, mode=excluded.mode`,
		b.Scope, b.LimitUSD, b.PeriodS, b.Mode)
	return err
}

func (s *Store) ListBudgets() ([]Budget, error) {
	rows, err := s.db.Query(`SELECT scope, limit_usd, period_s, mode, created_at FROM budgets ORDER BY scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Budget
	for rows.Next() {
		var b Budget
		if err := rows.Scan(&b.Scope, &b.LimitUSD, &b.PeriodS, &b.Mode, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetBudget(scope string) (Budget, bool, error) {
	var b Budget
	err := s.db.QueryRow(`SELECT scope, limit_usd, period_s, mode, created_at FROM budgets WHERE scope=?`,
		scope).Scan(&b.Scope, &b.LimitUSD, &b.PeriodS, &b.Mode, &b.CreatedAt)
	if err == sql.ErrNoRows {
		return Budget{}, false, nil
	}
	return b, err == nil, err
}

func (s *Store) DeleteBudget(scope string) error {
	_, err := s.db.Exec(`DELETE FROM budgets WHERE scope=?`, scope)
	return err
}

// Decision is the outcome of a budget check for one scope.
type Decision struct {
	Over      bool
	Mode      string
	Scope     string
	LimitUSD  float64
	SpentUSD  float64
	Exhausted []string
}

// BudgetCache holds pre-computed spend per scope, refreshed periodically so the
// request path never scans ai_requests (spec §9).
type BudgetCache struct {
	store   *Store
	clock   func() time.Time
	mu      sync.RWMutex
	decided map[string]Decision // scope -> decision
	groupOf map[string]string   // agent -> group (for Check)
	agents  map[string]Decision // agent -> calendar budget decision
}

func NewBudgetCache(s *Store, clock func() time.Time) *BudgetCache {
	if clock == nil {
		clock = time.Now
	}
	return &BudgetCache{store: s, clock: clock, decided: map[string]Decision{}, groupOf: map[string]string{}, agents: map[string]Decision{}}
}

// Refresh recomputes each budget's spend over its rolling period. Group scopes
// aggregate the spend of their members (agents.group), and the agent->group map
// is cached so Check can fold a member's group decision in without a DB hit.
func (c *BudgetCache) Refresh() error {
	budgets, err := c.store.ListBudgets()
	if err != nil {
		return err
	}
	agentNames, err := c.store.ListAgentBudgetNames()
	if err != nil {
		return err
	}
	groupOf, err := c.store.AgentGroups()
	if err != nil {
		return err
	}
	membersByGroup := map[string][]string{}
	for ag, g := range groupOf {
		membersByGroup[g] = append(membersByGroup[g], ag)
	}
	now := c.clock()
	next := map[string]Decision{}
	nextAgents := map[string]Decision{}
	for _, b := range budgets {
		since := now.Add(-time.Duration(b.PeriodS) * time.Second)
		var spent float64
		switch {
		case b.Scope == "global":
			spent, err = c.store.CostSince("", since)
		case len(b.Scope) > 6 && b.Scope[:6] == "agent:":
			spent, err = c.store.CostSince(b.Scope[6:], since)
		case len(b.Scope) > 6 && b.Scope[:6] == "group:":
			spent, err = c.store.GroupCostSince(membersByGroup[b.Scope[6:]], since)
		default:
			continue
		}
		if err != nil {
			return err
		}
		next[b.Scope] = Decision{Over: spent >= b.LimitUSD, Mode: b.Mode, Scope: b.Scope,
			LimitUSD: b.LimitUSD, SpentUSD: spent}
	}
	for _, agent := range agentNames {
		status, err := c.store.AgentBudgetStatus(agent, now)
		if err != nil {
			return err
		}
		if len(status.Exhausted) > 0 {
			nextAgents[agent] = Decision{Over: true, Mode: "block", Scope: "agent:" + agent,
				LimitUSD: 0, SpentUSD: 0, Exhausted: status.Exhausted}
		}
	}
	c.mu.Lock()
	c.decided = next
	c.groupOf = groupOf
	c.agents = nextAgents
	c.mu.Unlock()
	return nil
}

// Check returns the worst applicable decision for an agent across its agent,
// group and global scopes. block beats warn. A non-over agent scope still
// yields a group/global block if either is over.
func (c *BudgetCache) Check(agent string) Decision {
	// Agent calendar limits must become effective as soon as costs or limits
	// are persisted. Keep the periodic cache for legacy global/group budgets,
	// but derive the agent-specific decision from durable accounting here.
	liveAgent := Decision{}
	liveAgentKnown := false
	if status, err := c.store.AgentBudgetStatus(agent, c.clock()); err == nil {
		liveAgentKnown = true
		if len(status.Exhausted) > 0 {
			liveAgent = Decision{Over: true, Mode: "block", Scope: "agent:" + agent, Exhausted: status.Exhausted}
		}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	scopes := []string{"agent:" + agent, "global"}
	if g := c.groupOf[agent]; g != "" {
		scopes = append(scopes, "group:"+g)
	}
	worst := Decision{}
	if liveAgent.Over {
		worst = liveAgent
	} else if !liveAgentKnown {
		// If durable accounting is unavailable, retain the last cached block
		// rather than accidentally allowing a request through on a DB fault.
		if d, ok := c.agents[agent]; ok {
			worst = d
		}
	}
	for _, scope := range scopes {
		if d, ok := c.decided[scope]; ok && d.Over {
			// block beats warn.
			if !worst.Over || (worst.Mode != "block" && d.Mode == "block") {
				worst = d
			}
		}
	}
	return worst
}
