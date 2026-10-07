package loop

import (
	"path/filepath"
	"testing"
	"time"
)

func TestObserveCursorStreamLineReadsResultUsage(t *testing.T) {
	initLine := `{"type":"system","subtype":"init","apiKeySource":"env","cwd":"/home/nybble","session_id":"c4aa724f-cb52-4dc4-8a37-eb298d64e1f3","model":"Grok 4.7 256K High","permissionMode":"default"}`
	model, usage, ok := observeCursorStreamLine(initLine)
	if !ok || model != "Grok 4.7 256K High" || usage != nil {
		t.Fatalf("init = %q %#v %v", model, usage, ok)
	}

	resultLine := `{"type":"result","subtype":"success","duration_ms":4708,"duration_api_ms":4708,"is_error":false,"result":"ok","session_id":"c4aa724f-cb52-4dc4-8a37-eb298d64e1f3","request_id":"6dd791f5-73f3-4c9a-8f94-323400fadbd2","usage":{"inputTokens":4841,"outputTokens":31,"cacheReadTokens":5888,"cacheWriteTokens":0}}`
	model, usage, ok = observeCursorStreamLine(resultLine)
	if !ok || model != "" || usage == nil {
		t.Fatalf("result = %q %#v %v", model, usage, ok)
	}
	if usage.InputTokens != 4841 || usage.OutputTokens != 31 || usage.CacheReadTokens != 5888 || usage.CacheWriteTokens != 0 || usage.DurationMs != 4708 {
		t.Fatalf("usage = %+v", usage)
	}

	for _, line := range []string{
		`{"type":"result","subtype":"success","result":"ok"}`,
		`{"model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":40}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
		`not-json`,
	} {
		if _, u, ok := observeCursorStreamLine(line); ok && u != nil {
			t.Fatalf("line counted as cursor usage: %s", line)
		}
	}
}

func TestTailerReportsCursorResultUsage(t *testing.T) {
	dir := t.TempDir()
	got := make(chan CursorResultUsage, 1)
	var model string
	tl := startTailer(nil, "it-1", dir, 5*time.Millisecond, false, func(line string) {
		m, usage, ok := observeCursorStreamLine(line)
		if !ok {
			return
		}
		if m != "" {
			model = m
			return
		}
		if usage != nil {
			usage.Model = model
			got <- *usage
		}
	})
	appendLine(t, filepath.Join(dir, "harness.stdout.log"), `{"type":"system","subtype":"init","model":"Grok 4.7 256K High"}`)
	appendLine(t, filepath.Join(dir, "harness.stdout.log"), `{"type":"result","subtype":"success","duration_ms":4708,"usage":{"inputTokens":4841,"outputTokens":31,"cacheReadTokens":5888,"cacheWriteTokens":0}}`)
	select {
	case usage := <-got:
		tl.Stop()
		if usage.Model != "Grok 4.7 256K High" || usage.InputTokens != 4841 || usage.OutputTokens != 31 || usage.CacheReadTokens != 5888 || usage.DurationMs != 4708 {
			t.Fatalf("usage = %+v", usage)
		}
	case <-time.After(2 * time.Second):
		tl.Stop()
		t.Fatal("timed out waiting for cursor usage")
	}
}
