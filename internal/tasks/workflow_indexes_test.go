package tasks

import (
	"strings"
	"testing"
)

// TestWorkflowRunQueriesUseIndexes pins the query plans of the worker's and
// the readers' run queries: none of them scans task_script_runs or
// task_status_visits.
func TestWorkflowRunQueriesUseIndexes(t *testing.T) {
	svc := newTestService(t)
	for name, tc := range map[string]struct {
		query string
		args  []any
	}{
		"pending runs":     {scriptRunSelect + pendingRunsWhere, nil},
		"cancel requested": {scriptRunSelect + cancelRequestedWhere, nil},
		"running runs":     {scriptRunSelect + runningRunsWhere, nil},
		"newest runs":      {scriptRunSelect + taskRunsWhere + ` LIMIT ?`, []any{1, workflowViewRuns}},
		"due watches":      {dueWatchVisits + ` ORDER BY v.next_watch_at, v.id`, []any{"2026-10-02T00:00:00.000000000Z"}},
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := svc.db.Query(`EXPLAIN QUERY PLAN `+tc.query, tc.args...)
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
			for _, step := range plan {
				if strings.HasPrefix(step, "SCAN ") {
					t.Fatalf("plan scans a table:\n%s", strings.Join(plan, "\n"))
				}
			}
		})
	}
}
