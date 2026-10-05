package workflowrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/alekzonder/tariboy/internal/script"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// Bounds on the result file of a source script.
const (
	MaxSourceResultBytes      = 1 << 20
	MaxSourceItems            = 50
	MaxSourceKeyBytes         = 200
	MaxSourceTitleBytes       = 256
	MaxSourceDescriptionBytes = 16 << 10
)

var sourceKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:@/#-]+$`)

// sourceResult is the JSON object a source script may write.
type sourceResult struct {
	Items []tasks.SourceItem `json:"items"`
}

// classifySource turns the exit code and result file of a finished source run
// into a verdict. The result is accepted whole or not at all.
func classifySource(code int, raw []byte, readErr error, declared Declared) Verdict {
	switch {
	case code == script.QuietExit:
		return Verdict{Kind: VerdictQuiet}
	case code != 0:
		return failure("the script failed with exit %d", code)
	}
	items, err := readSourceResult(raw, readErr, declared)
	if err != nil {
		return Verdict{Kind: VerdictFailure, Message: cutRunes(err.Error(), MaxMessageBytes)}
	}
	return Verdict{Kind: VerdictItems, Items: items}
}

// readSourceResult parses and checks the items of a source result file. A
// missing or blank file reports no items.
func readSourceResult(raw []byte, readErr error, declared Declared) ([]tasks.SourceItem, error) {
	if readErr != nil {
		return nil, fmt.Errorf("the result file could not be read: %w", readErr)
	}
	if len(raw) > MaxSourceResultBytes {
		return nil, fmt.Errorf("the result file is too large: %d bytes, at most %d", len(raw), MaxSourceResultBytes)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if !utf8.Valid(trimmed) {
		return nil, errors.New("the result file is not valid UTF-8")
	}
	if trimmed[0] != '{' {
		return nil, errors.New("the result file is not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var res sourceResult
	if err := dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("the result file is not valid: %s", cutRunes(err.Error(), maxDecodeErrorBytes))
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the result file has data after the JSON object")
	}
	if len(res.Items) > MaxSourceItems {
		return nil, fmt.Errorf("the result reports %d items, at most %d", len(res.Items), MaxSourceItems)
	}
	seen := map[string]bool{}
	for i := range res.Items {
		item := &res.Items[i]
		item.Title = strings.TrimSpace(item.Title)
		if err := checkSourceItem(*item, declared); err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		if seen[item.Key] {
			return nil, fmt.Errorf("item %d: key %q is repeated", i, item.Key)
		}
		seen[item.Key] = true
	}
	return res.Items, nil
}

func checkSourceItem(item tasks.SourceItem, declared Declared) error {
	switch {
	case item.Key == "":
		return errors.New("key is required")
	case len(item.Key) > MaxSourceKeyBytes:
		return fmt.Errorf("key is longer than %d bytes", MaxSourceKeyBytes)
	case !sourceKeyPattern.MatchString(item.Key):
		return fmt.Errorf("key %q must match %s", cutRunes(item.Key, maxEchoedNameBytes), sourceKeyPattern)
	case item.Title == "":
		return errors.New("title is required")
	case strings.ContainsAny(item.Title, "\r\n"):
		return errors.New("title must be one line")
	case len(item.Title) > MaxSourceTitleBytes:
		return fmt.Errorf("title is longer than %d bytes", MaxSourceTitleBytes)
	case len(item.Description) > MaxSourceDescriptionBytes:
		return fmt.Errorf("description is longer than %d bytes", MaxSourceDescriptionBytes)
	}
	if _, err := tasks.NormalizePriority(item.Priority); err != nil {
		return err
	}
	return checkArtifacts(item.Artifacts, declared.Artifacts)
}
