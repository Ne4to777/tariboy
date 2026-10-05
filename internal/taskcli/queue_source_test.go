package taskcli

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

func TestQueueSourceCommandsCallTheirRoutes(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{"sources":[],"count":0}`)}
	withCaller(t, r)
	for _, tc := range []struct {
		argv  []string
		route string
		body  map[string]any
	}{
		{[]string{"queue", "source", "ls", "DEV"}, "/api/task-queues/DEV/sources", map[string]any{}},
		{[]string{"queue", "source", "log", "DEV", "12", "--max-bytes", "4096"}, "/api/task-queues/DEV/source-runs/12/log",
			map[string]any{"max_bytes": "4096"}},
	} {
		r.calls = nil
		if code := Run(context.Background(), tc.argv, operatorEnv(t), io.Discard, io.Discard); code != 0 {
			t.Fatalf("%v: code %d", tc.argv, code)
		}
		if len(r.calls) != 1 || r.calls[0].method != "GET" || r.calls[0].route != tc.route || !sameJSON(r.calls[0].body, tc.body) {
			t.Fatalf("%v: calls = %#v; want GET %s %v", tc.argv, r.calls, tc.route, tc.body)
		}
	}
}
