// Package workflowrun runs the scripts of a workflow image. This file holds the
// protocol between the daemon and a script: the environment a run receives,
// how its result file is parsed and bounded, and how an exit code plus result
// file become a verdict. It starts no process and touches no database.
package workflowrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// Kinds of script a run executes.
const (
	KindCheck  = "check"
	KindWatch  = "watch"
	KindSource = "source"
)

// Verdicts a finished run can have.
const (
	VerdictPass    = "pass"    // check: condition holds
	VerdictReject  = "reject"  // check: condition does not hold
	VerdictOutcome = "outcome" // watch: an outcome is ready
	VerdictQuiet   = "quiet"   // watch or source: nothing changed
	VerdictItems   = "items"   // source: the items it found
	VerdictFailure = "failure" // anything else
)

// Bounds on what a script may write to its result file.
const (
	MaxResultBytes   = 64 << 10
	MaxMessageBytes  = 4 << 10
	MaxArtifactBytes = 64 << 10
)

const defaultRejectMessage = "the check's condition does not hold"

// Declared lists what the workflow lets a result name.
type Declared struct {
	Outcomes  []string // outcomes of the current status; watch only
	Artifacts []string // artifact names the workflow declares
}

// Verdict is what a finished run means.
type Verdict struct {
	Kind      string             // one of the Verdict* constants
	Outcome   string             // set for VerdictOutcome
	Message   string             // script message, or the failure reason
	Artifacts map[string]string  // set for VerdictPass and VerdictOutcome
	Items     []tasks.SourceItem // set for VerdictItems
}

// resultFile is the JSON object a script may write.
type resultFile struct {
	Outcome   string            `json:"outcome"`
	Message   string            `json:"message"`
	Artifacts map[string]string `json:"artifacts"`
}

func failure(format string, args ...any) Verdict {
	return Verdict{Kind: VerdictFailure, Message: fmt.Sprintf(format, args...)}
}

// Classify turns a finished process into a verdict. exit is nil when the
// process did not exit normally; timedOut and rawResult describe the rest.
// rawResult is nil, with a nil resultErr, when the script wrote no result file.
func Classify(kind string, exit *int, timedOut bool, rawResult []byte, resultErr error, declared Declared) Verdict {
	if kind != KindCheck && kind != KindWatch && kind != KindSource {
		return failure("unknown script kind %q", kind)
	}
	if timedOut {
		return failure("the script timed out")
	}
	if exit == nil {
		return failure("the script did not exit normally")
	}
	code := *exit
	if kind == KindSource {
		return classifySource(code, rawResult, resultErr, declared)
	}

	// A quiet watch run publishes nothing, so its result file is not read.
	if kind == KindWatch && code == script.QuietExit {
		return Verdict{Kind: VerdictQuiet}
	}

	res, err := readResult(rawResult, resultErr)

	switch {
	case code == 0:
		if err != nil {
			return failure("%v", err)
		}
		return classifySuccess(kind, res, declared)
	case kind == KindCheck && code == script.RejectExit:
		if err != nil {
			return failure("%v", err)
		}
		if err := checkMessage(res.Message); err != nil {
			return failure("%v", err)
		}
		msg := res.Message
		if msg == "" {
			msg = defaultRejectMessage
		}
		return Verdict{Kind: VerdictReject, Message: msg}
	case kind == KindCheck && code == script.QuietExit:
		return failure("exit %d is the quiet code and is not valid for a check", code)
	case kind == KindWatch && code == script.RejectExit:
		return failure("exit %d is the reject code and is not valid for a watch script", code)
	}
	msg := "the script failed with exit " + strconv.Itoa(code)
	if err == nil && res.Message != "" {
		msg += ": " + res.Message
	}
	return Verdict{Kind: VerdictFailure, Message: cutRunes(msg, MaxMessageBytes)}
}

func classifySuccess(kind string, res resultFile, declared Declared) Verdict {
	if err := checkMessage(res.Message); err != nil {
		return failure("%v", err)
	}
	if err := checkArtifacts(res.Artifacts, declared.Artifacts); err != nil {
		return failure("%v", err)
	}
	if kind == KindCheck {
		return Verdict{Kind: VerdictPass, Message: res.Message, Artifacts: res.Artifacts}
	}
	if res.Outcome == "" {
		return failure("the watch script exited 0 without an outcome")
	}
	if !contains(declared.Outcomes, res.Outcome) {
		return failure("the watch script reported undeclared outcome %q", res.Outcome)
	}
	return Verdict{Kind: VerdictOutcome, Outcome: res.Outcome, Message: res.Message, Artifacts: res.Artifacts}
}

// readResult parses the result file. A missing or blank file is an empty
// result; a read error, an oversized file, or an invalid one is an error.
func readResult(raw []byte, readErr error) (resultFile, error) {
	var res resultFile
	if readErr != nil {
		return res, fmt.Errorf("the result file could not be read: %w", readErr)
	}
	if len(raw) > MaxResultBytes {
		return res, fmt.Errorf("the result file is too large: %d bytes, at most %d", len(raw), MaxResultBytes)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return res, nil
	}
	// The JSON decoder replaces invalid bytes silently, so check them here.
	if !utf8.Valid(trimmed) {
		return res, errors.New("the result file is not valid UTF-8")
	}
	if trimmed[0] != '{' {
		return res, errors.New("the result file is not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := decodeResult(dec, &res); err != nil {
		return resultFile{}, fmt.Errorf("the result file is not valid: %s", cutRunes(err.Error(), maxDecodeErrorBytes))
	}
	if _, err := dec.Token(); err != io.EOF {
		return resultFile{}, errors.New("the result file has data after the JSON object")
	}
	return res, nil
}

// maxDecodeErrorBytes bounds the decoder error text in a failure message: it
// may quote the script's input.
const maxDecodeErrorBytes = 256

// maxEchoedNameBytes bounds a key or artifact name from the result file that
// goes into a failure message.
const maxEchoedNameBytes = 64

// decodeResult reads one JSON object into res, matching its keys exactly,
// unlike encoding/json, which matches them without regard to case. A key that
// is not outcome, message, or artifacts, or that appears twice, is an error.
func decodeResult(dec *json.Decoder, res *resultFile) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	seen := map[string]bool{}
	for dec.More() {
		key, err := objectKey(dec, seen)
		if err != nil {
			return err
		}
		switch key {
		case "outcome":
			err = dec.Decode(&res.Outcome)
		case "message":
			err = dec.Decode(&res.Message)
		case "artifacts":
			res.Artifacts, err = decodeArtifacts(dec)
		default:
			err = fmt.Errorf("unknown field %q", cutRunes(key, maxEchoedNameBytes))
		}
		if err != nil {
			return err
		}
	}
	return expectDelim(dec, '}')
}

// decodeArtifacts reads the artifacts value: null, or an object of strings
// whose names appear once each.
func decodeArtifacts(dec *json.Decoder) (map[string]string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	if tok != json.Delim('{') {
		return nil, errors.New("artifacts is not an object")
	}
	artifacts := map[string]string{}
	seen := map[string]bool{}
	for dec.More() {
		name, err := objectKey(dec, seen)
		if err != nil {
			return nil, err
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("artifact %q: %v", cutRunes(name, maxEchoedNameBytes), err)
		}
		artifacts[name] = value
	}
	return artifacts, expectDelim(dec, '}')
}

// objectKey reads the next key of an object and refuses one already in seen.
func objectKey(dec *json.Decoder, seen map[string]bool) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	key, ok := tok.(string)
	if !ok {
		return "", errors.New("an object key is not a string")
	}
	if seen[key] {
		return "", fmt.Errorf("duplicate key %q", cutRunes(key, maxEchoedNameBytes))
	}
	seen[key] = true
	return key, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != want {
		return fmt.Errorf("expected %v", want)
	}
	return nil
}

// cutRunes cuts s to at most n bytes on a rune boundary.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func checkMessage(m string) error {
	if len(m) > MaxMessageBytes {
		return fmt.Errorf("the result message is too large: %d bytes, at most %d", len(m), MaxMessageBytes)
	}
	return nil
}

func checkArtifacts(got map[string]string, declared []string) error {
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := got[name]
		switch {
		case !contains(declared, name):
			return fmt.Errorf("the result names undeclared artifact %q", cutRunes(name, maxEchoedNameBytes))
		case v == "":
			return fmt.Errorf("artifact %q is empty", name)
		case !utf8.ValidString(v):
			return fmt.Errorf("artifact %q is not valid UTF-8", name)
		case len(v) > MaxArtifactBytes:
			return fmt.Errorf("artifact %q is too large: %d bytes, at most %d", name, len(v), MaxArtifactBytes)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// EnvValues are the per-run values behind the TARIBOY_* variables.
type EnvValues struct {
	TaskKey, Queue, WorkflowName, WorkflowVersion, Status, Outcome string
	WorkflowDir, TaskFile, TaskDir, ResultFile                     string
}

// ProtocolEnv returns the TARIBOY_* entries for one run, sorted by name.
// TARIBOY_WORKFLOW_OUTCOME is present only when Outcome is set.
func ProtocolEnv(v EnvValues) []string {
	pairs := map[string]string{
		"TARIBOY_TASK_KEY":         v.TaskKey,
		"TARIBOY_TASK_QUEUE":       v.Queue,
		"TARIBOY_WORKFLOW_NAME":    v.WorkflowName,
		"TARIBOY_WORKFLOW_VERSION": v.WorkflowVersion,
		"TARIBOY_WORKFLOW_STATUS":  v.Status,
		"TARIBOY_WORKFLOW_DIR":     v.WorkflowDir,
		"TARIBOY_TASK_FILE":        v.TaskFile,
		"TARIBOY_TASK_DIR":         v.TaskDir,
		"TARIBOY_RESULT_FILE":      v.ResultFile,
		"TARIBOY_QUIET_EXIT":       strconv.Itoa(script.QuietExit),
		"TARIBOY_REJECT_EXIT":      strconv.Itoa(script.RejectExit),
	}
	if v.Outcome != "" {
		pairs["TARIBOY_WORKFLOW_OUTCOME"] = v.Outcome
	}
	names := make([]string, 0, len(pairs))
	for name := range pairs {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+pairs[name])
	}
	return env
}
