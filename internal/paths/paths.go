// Package paths resolves the tariboy base directory and its layout.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// MaxSockPath is the supported cross-platform sockaddr_un.sun_path ceiling:
// macOS has 104 bytes including the NUL terminator, so paths are <= 103.
const MaxSockPath = 103

type Paths struct {
	Base string
	// Runtime is a SHORT directory that holds unix sockets. Sockets hit the OS
	// sun_path limit (see MaxSockPath), and Base may be arbitrarily long or deep
	// (a temp dir, a nested workspace), so sockets never live under Base. Runtime
	// is rooted in $HOME and keyed by a hash of Base so daemons with different
	// base dirs don't collide on the same socket names.
	Runtime string
}

// sockNameReserve is the longest per-agent socket leaf we budget for
// ("/<name>.shim.sock") when deciding whether sockets fit under the base dir.
// Keeping the base under (MaxSockPath - sockNameReserve) guarantees agent
// sockets with names up to ~30 chars stay bindable.
const sockNameReserve = 40

// runtimeFor picks the socket directory. When the base dir is short enough that
// the longest socket path still fits under the OS sun_path limit, sockets live
// under base (the stable, well-known location, and hermetic for tests). Only a
// base too long to hold a bindable socket relocates them to a short home-rooted
// dir keyed by a hash of base (so different base dirs don't collide). HOME unset
// leaves them under base; the startup BindableSocketPath check then flags it.
func runtimeFor(base string, getenv func(string) string) string {
	if r := getenv("TARIBOY_RUNTIME_DIR"); r != "" {
		return r
	}
	if len(base)+sockNameReserve <= MaxSockPath {
		return base
	}
	home := getenv("HOME")
	if home == "" {
		return base
	}
	sum := sha256.Sum256([]byte(base))
	return filepath.Join(home, ".tariboy", "run", hex.EncodeToString(sum[:])[:8])
}

// New builds Paths for an explicit base dir, resolving the runtime socket dir
// from the process environment. Use this when the base dir is already known
// (e.g. the daemon's --base-dir flag) instead of re-deriving it via Resolve.
func New(base string) Paths {
	return Paths{Base: base, Runtime: runtimeFor(base, os.Getenv)}
}

// Resolve picks $TARIBOY_BASE_DIR (else $HOME/.tariboy) for data, and the
// daemon runtime dir (control socket, pidfile, log) from $TARIBOY_RUNTIME_DIR
// (else $HOME/.tariboyd). getenv is injected for testability. This is the
// production path used by both the daemon (no --base-dir) and the CLI, so they
// agree on the control socket regardless of how long the base data dir is.
func Resolve(getenv func(string) string) (Paths, error) {
	base := getenv("TARIBOY_BASE_DIR")
	if base == "" {
		h := getenv("HOME")
		if h == "" {
			return Paths{}, errors.New("cannot resolve base dir: neither TARIBOY_BASE_DIR nor HOME is set")
		}
		base = filepath.Join(h, ".tariboy")
	}
	runtime := getenv("TARIBOY_RUNTIME_DIR")
	if runtime == "" {
		if h := getenv("HOME"); h != "" {
			runtime = filepath.Join(h, ".tariboyd")
		} else {
			runtime = base // BindableSocketPath flags a pathological layout later
		}
	}
	return Paths{Base: base, Runtime: runtime}, nil
}

func (p Paths) DB() string { return filepath.Join(p.Base, "tariboyd.db") }
func (p Paths) ProxyHandoffFile() string {
	return filepath.Join(p.Base, "aiproxy-handoff.json")
}
func (p Paths) PricingCatalogFile() string {
	return filepath.Join(p.Base, "model-prices-litellm.json")
}
func (p Paths) Socket() string    { return filepath.Join(p.RuntimeDir(), "tariboyd.sock") }
func (p Paths) PidFile() string   { return filepath.Join(p.RuntimeDir(), "tariboyd.pid") }
func (p Paths) LogFile() string   { return filepath.Join(p.RuntimeDir(), "tariboyd.log") }
func (p Paths) AgentsDir() string { return filepath.Join(p.Base, "agents") }
func (p Paths) ImagesDir() string { return filepath.Join(p.Base, "images") }
func (p Paths) ImageSourcesDir() string {
	return filepath.Join(p.Base, "image-sources")
}
func (p Paths) PluginsDir() string { return filepath.Join(p.Base, "plugins") }

// WorkflowsDir is the workflow image store.
func (p Paths) WorkflowsDir() string { return filepath.Join(p.Base, "workflows") }

// RuntimeDir returns the socket directory, falling back to Base for a
// zero-value Paths that was constructed without a runtime dir.
func (p Paths) RuntimeDir() string {
	if p.Runtime != "" {
		return p.Runtime
	}
	return p.Base
}

// BindableSocketPath returns an error naming the offending path if it is too
// long to bind as a unix socket. Call this at startup so an unbindable layout
// fails loudly with an actionable message instead of an opaque EINVAL deep in a
// goroutine.
func BindableSocketPath(sock string) error {
	if len(sock) > MaxSockPath {
		return fmt.Errorf("socket path is %d bytes, over the OS limit of %d: %s", len(sock), MaxSockPath, sock)
	}
	return nil
}

func (p Paths) EnsureBase() error {
	dirs := []string{
		p.Base,
		p.AgentsDir(),
		p.ImagesDir(),
		p.ImageSourcesDir(),
		p.PluginsDir(),
		p.WorkflowsDir(),
		p.RuntimeDir(),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
