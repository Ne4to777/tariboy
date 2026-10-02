package script

import (
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

func TestProtocolConstants(t *testing.T) {
	if QuietExit != 111 || RejectExit != 112 {
		t.Fatalf("QuietExit=%d RejectExit=%d, want 111 and 112", QuietExit, RejectExit)
	}
	if got, want := ProtocolEnv(), []string{"TARIBOY_QUIET_EXIT=111"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProtocolEnv()=%#v, want %#v", got, want)
	}
}

func TestLegacyQuietCommandText(t *testing.T) {
	got := LegacyQuietCommand("poll --state 'a b'", 2)
	want := "sh -c 'poll --state '\\''a b'\\'''\n" +
		"__tariboy_rc=$?\n" +
		"[ \"$__tariboy_rc\" -eq 2 ] && exit 111\n" +
		"exit \"$__tariboy_rc\""
	if got != want {
		t.Fatalf("wrapped command:\n%s\nwant:\n%s", got, want)
	}
}

func shellExit(t *testing.T, command string) int {
	t.Helper()
	err := exec.Command("sh", "-c", command).Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run %q: %v", command, err)
	}
	return exit.ExitCode()
}

func TestDaemonAccessEnvNamesEverySocketAndToken(t *testing.T) {
	want := []string{"TARIBOY_TOOLS_SOCKET", "TARIBOY_DAEMON_SOCKET", "TARIBOY_PLUGIN_SOCKET", "TARIBOY_PLUGIN_TOKEN"}
	if !reflect.DeepEqual(DaemonAccessEnv, want) {
		t.Fatalf("DaemonAccessEnv = %v, want %v", DaemonAccessEnv, want)
	}
}

func TestLegacyQuietCommandMapsOnlyTheLegacyCode(t *testing.T) {
	cases := []struct {
		name    string
		command string
		code    int
		want    int
	}{
		{"legacy quiet code becomes 111", "exit 2", 2, 111},
		{"success passes through", "true", 2, 0},
		{"other failure passes through", "exit 3", 2, 3},
		{"already 111 stays 111", "exit 111", 2, 111},
		{"legacy code zero turns success into 111", "true", 0, 111},
		{"legacy code zero leaves failure alone", "exit 4", 0, 4},
		{"single quotes", "test 'a b' = 'a b' && exit 2", 2, 111},
		{"several lines", "x=1\ny=1\ntest \"$x\" = \"$y\" && exit 2", 2, 111},
		{"heredoc", "cat <<'EOF' >/dev/null\nit's a line\nEOF\nexit 2", 2, 111},
		{"trailing backslash", "true \\", 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellExit(t, LegacyQuietCommand(tc.command, tc.code)); got != tc.want {
				t.Fatalf("wrapped %q exits %d, want %d", tc.command, got, tc.want)
			}
		})
	}
}
