// Package workflowfile parses and validates Workflowfile.yaml, the manifest of
// a workflow image source directory.
package workflowfile

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const DefaultFilename = "Workflowfile.yaml"

const (
	OwnerPool     = "pool"
	OwnerCustomer = "customer"
	OwnerScript   = "script"

	RunAsQueue = "queue"
	RunAsAgent = "agent"
)

type File struct {
	SchemaVersion   int               `yaml:"schema_version" json:"schema_version"`
	Name            string            `yaml:"name" json:"name"`
	WorkflowVersion string            `yaml:"workflow_version" json:"workflow_version"`
	InitialStatus   string            `yaml:"initial_status" json:"initial_status"`
	RequiresSecrets []string          `yaml:"requires_secrets,omitempty" json:"requires_secrets,omitempty"`
	Env             map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Limits          *Limits           `yaml:"limits,omitempty" json:"limits,omitempty"`
	Artifacts       []Artifact        `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	Sources         []Source          `yaml:"sources,omitempty" json:"sources,omitempty"`
	Statuses        []Status          `yaml:"statuses" json:"statuses"`
	Dir             string            `yaml:"-" json:"-"` // directory holding the manifest
}

type Limits struct {
	IdleIterations   *int   `yaml:"idle_iterations,omitempty" json:"idle_iterations,omitempty"`
	RejectedRequests *int   `yaml:"rejected_requests,omitempty" json:"rejected_requests,omitempty"`
	ScriptFailures   *int   `yaml:"script_failures,omitempty" json:"script_failures,omitempty"`
	UnavailableGrace string `yaml:"unavailable_grace,omitempty" json:"unavailable_grace,omitempty"`
}

type Artifact struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Owner is the owner of a status. YAML has three forms: the scalar customer,
// the scalar script, or the mapping {pool: NAME}. It has no yaml tags because
// UnmarshalYAML and MarshalYAML implement those forms.
type Owner struct {
	Kind string `json:"kind"`           // OwnerPool, OwnerCustomer, OwnerScript, or "" when absent
	Pool string `json:"pool,omitempty"` // set only for OwnerPool
}

type Status struct {
	ID           string       `yaml:"id" json:"id"`
	Owner        Owner        `yaml:"owner,omitempty" json:"owner"`
	Instructions string       `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	Watch        *Watch       `yaml:"watch,omitempty" json:"watch,omitempty"`
	Transitions  []Transition `yaml:"transitions,omitempty" json:"transitions,omitempty"`
	Limits       *Limits      `yaml:"limits,omitempty" json:"limits,omitempty"`
	Terminal     bool         `yaml:"terminal,omitempty" json:"terminal,omitempty"`
	Cancelled    bool         `yaml:"cancelled,omitempty" json:"cancelled,omitempty"`
}

type Watch struct {
	Script  string `yaml:"script" json:"script"`
	Every   string `yaml:"every" json:"every"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Source is a queue-level script the daemon runs on a schedule for every queue
// bound to the workflow. Each new item key it reports becomes a task.
type Source struct {
	Name    string `yaml:"name" json:"name"`
	Script  string `yaml:"script" json:"script"`
	Every   string `yaml:"every" json:"every"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type Transition struct {
	On       string   `yaml:"on" json:"on"`
	To       string   `yaml:"to" json:"to"`
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty"`
	Checks   []Check  `yaml:"checks,omitempty" json:"checks,omitempty"`
}

type Check struct {
	Script  string `yaml:"script" json:"script"`
	RunAs   string `yaml:"run_as,omitempty" json:"run_as,omitempty"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Parse reads a Workflowfile.yaml, or the one inside a directory. It decodes
// strictly and sets Dir. It does not validate the graph; call Validate. A file
// under any other name is refused, because the build stores the file named
// Workflowfile.yaml and its bytes must be the ones parsed.
func Parse(path string) (*File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		path = filepath.Join(path, DefaultFilename)
	} else if filepath.Base(path) != DefaultFilename {
		return nil, fmt.Errorf("parse %s: the manifest must be named %s", path, DefaultFilename)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var out File
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents are not allowed")
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out.Dir = filepath.Dir(path)
	return &out, nil
}

// UnmarshalYAML accepts customer, script, or {pool: NAME}. It rejects every
// other form, including unknown mapping keys.
func (o *Owner) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		switch node.Value {
		case OwnerCustomer, OwnerScript:
			*o = Owner{Kind: node.Value}
			return nil
		}
		return fmt.Errorf("line %d: owner %q must be customer, script, or {pool: NAME}", node.Line, node.Value)
	case yaml.MappingNode:
		var pool string
		seen := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			if key.Value != OwnerPool || seen {
				return fmt.Errorf("line %d: owner mapping allows only the key %q once, got %q", key.Line, OwnerPool, key.Value)
			}
			if val.Kind != yaml.ScalarNode || val.Value == "" || val.ShortTag() == "!!null" {
				return fmt.Errorf("line %d: owner pool must be a non-empty name", val.Line)
			}
			seen = true
			pool = val.Value
		}
		if !seen {
			return fmt.Errorf("line %d: owner mapping must be {pool: NAME}", node.Line)
		}
		*o = Owner{Kind: OwnerPool, Pool: pool}
		return nil
	}
	return fmt.Errorf("line %d: owner must be customer, script, or {pool: NAME}", node.Line)
}

// MarshalYAML writes the form UnmarshalYAML reads.
func (o Owner) MarshalYAML() (any, error) {
	switch o.Kind {
	case OwnerPool:
		return map[string]string{OwnerPool: o.Pool}, nil
	case OwnerCustomer, OwnerScript:
		return o.Kind, nil
	}
	return nil, nil
}
