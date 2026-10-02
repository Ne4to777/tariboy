package script

import (
	"strconv"
	"strings"
)

// Exit codes with a fixed meaning in the script protocol. Both lie in 100–113,
// a range that neither the shell, sysexits, nor Go's flag package uses, so a
// broken command cannot report either of them by accident.
const (
	// QuietExit means "nothing changed": a recurring run records its result,
	// publishes nothing, and keeps its schedule.
	QuietExit = 111
	// RejectExit is reserved for workflow checks: the script ran and its
	// condition does not hold. An agent script that exits with it has failed
	// like any other nonzero exit.
	RejectExit = 112
)

// QuietExitEnv names the variable that carries QuietExit into every run, so a
// script writes `exit "$TARIBOY_QUIET_EXIT"` instead of a magic number.
const QuietExitEnv = "TARIBOY_QUIET_EXIT"

// DaemonAccessEnv names the variables that reach the agent tools socket or the
// daemon API. A script that must not reach them, such as a workflow script or
// an agent script run without the tools socket, never receives any of them.
var DaemonAccessEnv = []string{
	"TARIBOY_TOOLS_SOCKET",
	"TARIBOY_DAEMON_SOCKET",
	"TARIBOY_PLUGIN_SOCKET",
	"TARIBOY_PLUGIN_TOKEN",
}

// ProtocolEnv returns the environment entries every script run receives.
// Append it after the agent's own environment so the protocol value wins.
func ProtocolEnv() []string {
	return []string{QuietExitEnv + "=" + strconv.Itoa(QuietExit)}
}

// LegacyQuietCommand wraps a command written for a configurable quiet exit
// code so that code is reported as QuietExit. The command runs in a nested
// shell with its text single-quoted, which keeps quotes, newlines, heredocs,
// and a trailing backslash inside it intact. Every other exit code, including
// QuietExit itself, passes through unchanged.
//
// Migration 0052_script_quiet_exit_constant.sql builds the same text in SQL;
// change both together.
func LegacyQuietCommand(command string, code int) string {
	quoted := "'" + strings.ReplaceAll(command, "'", `'\''`) + "'"
	return "sh -c " + quoted + "\n" +
		"__tariboy_rc=$?\n" +
		`[ "$__tariboy_rc" -eq ` + strconv.Itoa(code) + " ] && exit " + strconv.Itoa(QuietExit) + "\n" +
		`exit "$__tariboy_rc"`
}
