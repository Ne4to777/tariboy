package script

import "errors"

var (
	ErrNotFound = errors.New("not found")
	ErrActive   = errors.New("script is active")
	ErrMode     = errors.New("operation is not supported for this script mode")
	ErrConflict = errors.New("script state conflict")
)

const (
	ModeOnce  = "once"
	ModeEvery = "every"

	StateActive    = "active"
	StateCompleted = "completed"
	StateCancelled = "cancelled"

	RunPending     = "pending"
	RunRunning     = "running"
	RunSucceeded   = "succeeded"
	RunFailed      = "failed"
	RunCancelled   = "cancelled"
	RunTimedOut    = "timed_out"
	RunInterrupted = "interrupted"
)

type Definition struct {
	ID              string
	Agent           string
	Name            string
	Description     string
	Command         string
	Mode            string
	IntervalSeconds int
	State           string
	CreatedAt       string
	NextRunAt       string
	LatestRun       *Run
}

type Run struct {
	ID              string
	ScriptID        string
	Agent           string
	Status          string
	CancelRequested bool
	PID             *int
	ExitCode        *int
	CreatedAt       string
	StartedAt       string
	FinishedAt      string
	LogPath         string
}

type CreateOnce struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Command     string `json:"command"`
}

type CreateSchedule struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	Command         string `json:"command"`
	IntervalSeconds int    `json:"interval_seconds"`
	// QuietExit is deprecated input kept for one release. The quiet exit code
	// is the constant QuietExit; a different value here wraps the command with
	// LegacyQuietCommand so that code is reported as QuietExit. It is never
	// stored and never returned.
	QuietExit *int `json:"quiet_exit,omitempty"`
}

type Completion struct {
	Status     string
	ExitCode   *int
	FinishedAt string
	LogPath    string
}

type ResultPayload struct {
	ScriptID string `json:"script_id"`
	RunID    string `json:"run_id"`
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code,omitempty"`
	LogPath  string `json:"log_path"`
}
