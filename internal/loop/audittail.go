package loop

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Recorder is the slice of *audit.Log the tailer needs. Defined here (no audit
// import in the interface) so a single *audit.Log instance per agent can be
// shared by the daemon's recordEvent, the engine's audit sink, and this tailer —
// they must share one instance so the seq counter stays consistent.
type Recorder interface {
	Record(typ, source, iterationID string, data map[string]any) int64
}

// tailFile describes one log file the tailer tees into the audit log.
type tailFile struct {
	name   string // file basename under logsDir
	typ    string // audit event type
	source string // audit source
	stream string // data.stream ("" to omit, e.g. shim.log)
}

// shimTailFile is always teed. The harness stdout/stderr files are teed only for
// non-interactive (exec) agents — for interactive agents those files are the
// tmux pipe-pane capture (a full ANSI TUI-redraw firehose), which is noise in an
// audit log, so they are skipped.
var (
	shimTailFile     = tailFile{name: "shim.log", typ: "shim", source: "shim"}
	harnessTailFiles = []tailFile{
		{name: "harness.stdout.log", typ: "harness_output", source: "harness", stream: "stdout"},
		{name: "harness.stderr.log", typ: "harness_output", source: "harness", stream: "stderr"},
	}
)

// tailFilesFor returns the files to tee for an agent: shim.log always, plus the
// harness stdout/stderr streams for non-interactive agents only.
func tailFilesFor(interactive bool) []tailFile {
	files := []tailFile{shimTailFile}
	if !interactive {
		files = append(files, harnessTailFiles...)
	}
	return files
}

// Tailer streams an iteration's shim.log + harness.{stdout,stderr}.log into the
// audit log, one audit event per new line. It is best-effort: a missing file is
// skipped until it appears, read errors are swallowed, and it never blocks or
// panics into the loop. One tailer runs per iteration; Stop drains the tail and
// joins the goroutine.
type Tailer struct {
	rec      Recorder
	iterID   string
	logsDir  string
	poll     time.Duration
	files    []tailFile
	offsets  map[string]int64
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	onStdout func(string)
}

// StartTailer launches a Tailer goroutine and returns it. logsDir is the
// iteration's logs directory (agentdir Layout.LogsDir). When interactive, only
// shim.log is teed (the harness output is a tmux TUI capture, not audit-worthy).
func StartTailer(rec Recorder, iterationID, logsDir string, poll time.Duration, interactive bool) *Tailer {
	return startTailer(rec, iterationID, logsDir, poll, interactive, nil)
}

func startTailer(rec Recorder, iterationID, logsDir string, poll time.Duration, interactive bool, onStdout func(string)) *Tailer {
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	t := &Tailer{
		rec: rec, iterID: iterationID, logsDir: logsDir, poll: poll,
		files:    tailFilesFor(interactive),
		offsets:  map[string]int64{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		onStdout: onStdout,
	}
	go t.run()
	return t
}

func (t *Tailer) run() {
	defer close(t.done)
	tk := time.NewTicker(t.poll)
	defer tk.Stop()
	for {
		select {
		case <-t.stop:
			t.emit() // final drain: capture trailing lines written before Stop
			return
		case <-tk.C:
			t.emit()
		}
	}
}

// emit reads any bytes appended since the last offset for each file and records
// one audit event per whole line.
func (t *Tailer) emit() {
	for _, tf := range t.files {
		path := filepath.Join(t.logsDir, tf.name)
		f, err := os.Open(path)
		if err != nil {
			continue // not created yet, or transient — retry next tick
		}
		off := t.offsets[tf.name]
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			f.Close()
			continue
		}
		r := bufio.NewReader(f)
		var consumed int64
		for {
			line, nbytes, complete, err := readTailLine(r)
			if complete {
				consumed += int64(nbytes)
				if line != "" {
					text := trimNewline(line)
					if tf.stream == "stdout" && t.onStdout != nil {
						t.onStdout(text)
					}
					if t.rec != nil {
						data := map[string]any{"line": truncateAuditLine(text)}
						if tf.stream != "" {
							data["stream"] = tf.stream
						}
						t.rec.Record(tf.typ, tf.source, t.iterID, data)
					}
				}
			}
			if err != nil {
				break
			}
		}
		t.offsets[tf.name] = off + consumed
		f.Close()
	}
}

const (
	maxTailLineBytes  = 32 << 20
	maxAuditLineBytes = 8 << 10
)

func readTailLine(r *bufio.Reader) (line string, nbytes int, complete bool, err error) {
	var b strings.Builder
	overflow := false
	for {
		frag, readErr := r.ReadString('\n')
		nbytes += len(frag)
		if !overflow {
			if b.Len()+len(frag) <= maxTailLineBytes {
				b.WriteString(frag)
			} else {
				overflow = true
				b.Reset()
			}
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if overflow {
			ended := readErr == nil || strings.HasSuffix(frag, "\n")
			return "", nbytes, ended, readErr
		}
		text := b.String()
		ended := strings.HasSuffix(text, "\n")
		if !ended {
			return text, nbytes, false, readErr
		}
		return text, nbytes, true, nil
	}
}

func truncateAuditLine(s string) string {
	if len(s) <= maxAuditLineBytes {
		return s
	}
	s = s[:maxAuditLineBytes]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// Stop signals the tailer, waits (bounded) for the final drain, and returns.
// Safe to call more than once.
func (t *Tailer) Stop() {
	t.stopOnce.Do(func() { close(t.stop) })
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
	}
}
