package taskcli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestQueueSecretSetReadsStdinAndNeverEchoesTheValue(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{"queue":"DEV","key":"GH_TOKEN","updated_at":"now"}`)}
	withCaller(t, r)
	old := isTerminal
	t.Cleanup(func() { isTerminal = old })
	isTerminal = func(io.Reader) bool { return false }
	for input, want := range map[string]string{
		"tok-1\n":      "tok-1",
		"tok-2\r\n":    "tok-2",
		"tok-3":        "tok-3",
		"tok-4\n\n":    "tok-4\n",
		"--looks-like": "--looks-like",
	} {
		r.calls = nil
		withStdin(t, input)
		var out, errOut strings.Builder
		if code := Run(context.Background(), []string{"queue", "secret", "set", "DEV", "GH_TOKEN"}, operatorEnv(t), &out, &errOut); code != 0 {
			t.Fatalf("input %q: code %d: %s", input, code, &errOut)
		}
		if len(r.calls) != 1 || r.calls[0].method != "PUT" || r.calls[0].route != "/api/task-queues/DEV/secrets/GH_TOKEN" ||
			!sameJSON(r.calls[0].body, map[string]any{"value": want}) {
			t.Fatalf("input %q: calls = %#v; want PUT with value %q", input, r.calls, want)
		}
		if strings.Contains(out.String()+errOut.String(), strings.TrimSpace(want)) {
			t.Fatalf("input %q: output echoes the value: %q %q", input, &out, &errOut)
		}
	}
	// --value wins and stdin is not read.
	r.calls = nil
	withStdin(t, "ignored")
	if code := Run(context.Background(), []string{"queue", "secret", "set", "DEV", "GH_TOKEN", "--value", "from-flag"}, operatorEnv(t), io.Discard, io.Discard); code != 0 ||
		!sameJSON(r.calls[0].body, map[string]any{"value": "from-flag"}) {
		t.Fatalf("--value call = %d %#v", code, r.calls)
	}
}

func TestQueueSecretSetRefusesBadInputBeforeCalling(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{}`)}
	withCaller(t, r)
	old := isTerminal
	t.Cleanup(func() { isTerminal = old })
	argv := []string{"queue", "secret", "set", "DEV", "GH_TOKEN"}

	isTerminal = func(io.Reader) bool { return true }
	withStdin(t, "never read")
	var errOut strings.Builder
	if code := Run(context.Background(), argv, operatorEnv(t), io.Discard, &errOut); code != 2 || !strings.Contains(errOut.String(), "pipe the value on stdin") {
		t.Fatalf("terminal = %d %q", code, &errOut)
	}
	isTerminal = func(io.Reader) bool { return false }
	for name, input := range map[string]string{"empty": "", "only a newline": "\n", "binary": "ok \xff\xfe"} {
		withStdin(t, input)
		errOut.Reset()
		if code := Run(context.Background(), argv, operatorEnv(t), io.Discard, &errOut); code != 2 {
			t.Fatalf("%s = %d %q; want 2", name, code, &errOut)
		}
	}
	// A value as an argument would land in the shell history.
	errOut.Reset()
	if code := Run(context.Background(), append(append([]string{}, argv...), "s3cr3t"), operatorEnv(t), io.Discard, &errOut); code != 2 ||
		strings.Contains(errOut.String(), "s3cr3t") {
		t.Fatalf("positional value = %d %q", code, &errOut)
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls = %#v; want none", r.calls)
	}
}

func TestQueueSecretCommandsUseRestRoutes(t *testing.T) {
	for _, tt := range []struct {
		argv          []string
		method, route string
	}{
		{[]string{"queue", "secret", "ls", "DEV"}, "GET", "/api/task-queues/DEV/secrets"},
		{[]string{"queue", "secret", "rm", "DEV", "GH_TOKEN"}, "DELETE", "/api/task-queues/DEV/secrets/GH_TOKEN"},
	} {
		r := &recorder{result: json.RawMessage(`{"secrets":[{"key":"GH_TOKEN","updated_at":"now"}],"count":1}`)}
		withCaller(t, r)
		if code := Run(context.Background(), tt.argv, operatorEnv(t), io.Discard, io.Discard); code != 0 {
			t.Fatalf("%v: code %d", tt.argv, code)
		}
		if len(r.calls) != 1 || r.calls[0].method != tt.method || r.calls[0].route != tt.route {
			t.Fatalf("%v: calls = %#v", tt.argv, r.calls)
		}
	}
}

func TestQueueSecretCommandsAreRefusedInAgentMode(t *testing.T) {
	r := &recorder{result: json.RawMessage(`{}`)}
	withCaller(t, r)
	withStdin(t, "tok")
	for _, argv := range [][]string{
		{"queue", "secret", "set", "DEV", "GH_TOKEN", "--value", "x"},
		{"queue", "secret", "set", "DEV", "GH_TOKEN"},
		{"queue", "secret", "ls", "DEV"},
		{"queue", "secret", "rm", "DEV", "GH_TOKEN"},
	} {
		if code := Run(context.Background(), argv, agentEnv(), io.Discard, io.Discard); code != 2 {
			t.Errorf("Run(%q) = %d; want 2", argv, code)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("calls = %#v; want none", r.calls)
	}
}
