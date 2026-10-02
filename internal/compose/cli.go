package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/daemonctl"
	"github.com/alekzonder/tariboy/internal/teamportable"
)

// ensureDaemonUp is the auto-start seam (overridden in tests). It brings the
// starts the daemon detached before `compose up` applies the file.
var ensureDaemonUp = func(ctx context.Context, out io.Writer) error {
	cfg, err := daemonctl.ResolveConfig(os.Getenv)
	if err != nil {
		return err
	}
	_, err = daemonctl.EnsureUp(ctx, cfg, out)
	return err
}

// Load reads and parses (but does not validate) a compose file, returning it and
// the absolute directory it lives in (used to resolve relative image contexts,
// the $CWD token, and the default agent cwd).
func Load(path string) (File, string, error) {
	return loadComposeFile(path)
}

func loadComposeFile(path string) (File, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, "", fmt.Errorf("read %s: %w", path, err)
	}
	f, err := Parse(b)
	if err != nil {
		return File{}, "", err
	}
	dir := filepath.Dir(path)
	// Absolutize so $CWD and the default cwd (compose dir) resolve to a path the
	// daemon accepts (it requires an absolute, existing dir), regardless of
	// whether -f was given as a relative path.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return f, dir, nil
}

// Main is the `tariboy compose ...` entrypoint. It parses the verb + flags,
// loads the file, and drives the Runner. Returns a process exit code.
func Main(ctx context.Context, call Caller, imagesDir string, args []string, out, errOut io.Writer) int {
	file := "tariboy-compose.yaml"
	output := ""
	archive := ""
	yes := false
	volumes := false
	noStart := false
	tail := 200
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-f" || args[i] == "--file") && i+1 < len(args):
			i++
			file = args[i]
		case args[i] == "--output" && i+1 < len(args):
			i++
			output = args[i]
		case args[i] == "--archive" && i+1 < len(args):
			i++
			archive = args[i]
		case args[i] == "--yes":
			yes = true
		case args[i] == "--volumes" || args[i] == "-v":
			volumes = true
		case args[i] == "--no-start":
			noStart = true
		case args[i] == "--tail" && i+1 < len(args):
			i++
			fmt.Sscanf(args[i], "%d", &tail)
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 {
		fmt.Fprintln(errOut, "usage: tariboy compose <up|down|start|stop|restart|kill|exec|ps|status|logs|build|rm|archive|import> [-f file] [flags]")
		return 2
	}
	verb := rest[0]
	verbArgs := rest[1:]
	if verb == "archive" {
		if output == "" {
			fmt.Fprintln(errOut, "compose archive: --output is required")
			return 2
		}
		archiveOut, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			fmt.Fprintf(errOut, "compose archive: %v\n", err)
			return 1
		}
		if err := teamportable.CreateFromCompose(file, archiveOut); err != nil {
			_ = archiveOut.Close()
			_ = os.Remove(output)
			fmt.Fprintf(errOut, "compose archive: %v\n", err)
			return 1
		}
		if err := archiveOut.Close(); err != nil {
			_ = os.Remove(output)
			fmt.Fprintf(errOut, "compose archive: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, output)
		return 0
	}
	if verb == "import" {
		if archive == "" {
			fmt.Fprintln(errOut, "compose import: --archive is required")
			return 2
		}
		uploader, ok := call.(interface {
			Upload(string, io.Reader, int64) (json.RawMessage, error)
		})
		if !ok {
			fmt.Fprintln(errOut, "compose import: archive upload is not available")
			return 2
		}
		in, err := os.Open(archive)
		if err != nil {
			fmt.Fprintf(errOut, "compose import: %v\n", err)
			return 1
		}
		defer in.Close()
		info, err := in.Stat()
		if err != nil {
			fmt.Fprintf(errOut, "compose import: %v\n", err)
			return 1
		}
		preview, err := uploader.Upload("/api/team-imports", in, info.Size())
		if err != nil {
			fmt.Fprintf(errOut, "compose import: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, string(preview))
		var planned struct {
			ImportID string `json:"import_id"`
		}
		if err := json.Unmarshal(preview, &planned); err != nil || planned.ImportID == "" {
			fmt.Fprintln(errOut, "compose import: preview did not return an import id")
			return 1
		}
		if !yes {
			fmt.Fprint(errOut, "Apply this import? [y/N] ")
			var answer string
			if _, err := fmt.Fscan(os.Stdin, &answer); err != nil || (answer != "y" && answer != "Y" && answer != "yes") {
				fmt.Fprintln(errOut, "import not applied")
				return 0
			}
		}
		result, err := call.Call("POST", "/api/team-imports/"+planned.ImportID+"/apply", map[string]any{})
		if err != nil {
			fmt.Fprintf(errOut, "compose import: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, string(result))
		return 0
	}

	f, workdir, err := loadComposeFile(file)
	if err != nil {
		fmt.Fprintf(errOut, "compose: %v\n", err)
		return 1
	}
	if err := f.Validate(); err != nil {
		fmt.Fprintf(errOut, "compose: %v\n", err)
		return 1
	}
	r := NewRunner(call, imagesDir, workdir, out)

	// Auto-start the daemon before applying the file (default on). --no-start or
	// TARIBOY_NO_AUTOSTART restores the plain "not running" error path below.
	if verb == "up" && !noStart && os.Getenv("TARIBOY_NO_AUTOSTART") == "" {
		if err := ensureDaemonUp(ctx, out); err != nil {
			fmt.Fprintf(errOut, "compose up: %v\n", err)
			return 1
		}
	}

	var runErr error
	switch verb {
	case "up":
		runErr = r.Up(f)
	case "down":
		runErr = r.Down(f, volumes)
	case "build":
		runErr = r.Build(f)
	case "ps":
		runErr = r.Ps(f)
	case "status":
		runErr = r.Status(f)
	case "start", "stop", "restart", "kill":
		runErr = r.Lifecycle(f, verb, verbArgs)
	case "rm":
		runErr = r.Rm(f, verbArgs)
	case "exec":
		runErr = r.Exec(f, verbArgs)
	case "logs":
		runErr = r.Logs(f, verbArgs, tail)
	default:
		fmt.Fprintf(errOut, "compose: unknown verb %q\n", verb)
		return 2
	}
	if runErr != nil {
		if client.IsDaemonDown(runErr) {
			fmt.Fprintln(errOut, "tariboyd is not running (start it with: tariboyd)")
			return 2
		}
		fmt.Fprintf(errOut, "compose %s: %v\n", verb, runErr)
		return 1
	}
	return 0
}
