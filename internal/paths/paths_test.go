package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveExplicitBase(t *testing.T) {
	p, err := Resolve(env(map[string]string{"TARIBOY_BASE_DIR": "/tmp/sa"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Base != "/tmp/sa" {
		t.Fatalf("Base = %q", p.Base)
	}
	if got := p.Socket(); got != "/tmp/sa/tariboyd.sock" {
		t.Fatalf("Socket = %q", got)
	}
	if got := p.DB(); got != "/tmp/sa/tariboyd.db" {
		t.Fatalf("DB = %q", got)
	}
	if got := p.ProxyHandoffFile(); got != "/tmp/sa/aiproxy-handoff.json" {
		t.Fatalf("ProxyHandoffFile = %q", got)
	}
	if got := p.PricingCatalogFile(); got != "/tmp/sa/model-prices-litellm.json" {
		t.Fatalf("PricingCatalogFile = %q", got)
	}
}

func TestResolveHomeFallback(t *testing.T) {
	p, err := Resolve(env(map[string]string{"HOME": "/home/u"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Base != "/home/u/.tariboy" {
		t.Fatalf("Base = %q", p.Base)
	}
}

func TestResolveNoEnv(t *testing.T) {
	if _, err := Resolve(env(nil)); err == nil {
		t.Fatal("want error when no TARIBOY_BASE_DIR and no HOME")
	}
}

func TestResolveRuntimeDefaultsToTariboyd(t *testing.T) {
	p, err := Resolve(env(map[string]string{"HOME": "/home/u"}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.RuntimeDir(), "/home/u/.tariboyd"; got != want {
		t.Fatalf("runtime dir = %q, want %q", got, want)
	}
	if got, want := p.Socket(), "/home/u/.tariboyd/tariboyd.sock"; got != want {
		t.Fatalf("socket = %q, want %q", got, want)
	}
	if got, want := p.PidFile(), "/home/u/.tariboyd/tariboyd.pid"; got != want {
		t.Fatalf("pidfile = %q, want %q", got, want)
	}
	if got, want := p.LogFile(), "/home/u/.tariboyd/tariboyd.log"; got != want {
		t.Fatalf("logfile = %q, want %q", got, want)
	}
}

func TestResolveRuntimeEnvOverride(t *testing.T) {
	p, err := Resolve(env(map[string]string{"HOME": "/home/u", "TARIBOY_RUNTIME_DIR": "/tmp/rt"}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Socket(), "/tmp/rt/tariboyd.sock"; got != want {
		t.Fatalf("socket = %q, want %q", got, want)
	}
}

func TestRuntimeDirUnderHome(t *testing.T) {
	// A long base data dir must not push sockets over the OS limit.
	longBase := "/place/vartmp/claude-49433/-home-customer-github-tariboy/e798b187-2eee-4a1c-baab-4b2106920174/scratchpad/sa-base"

	// Resolve: the daemon control socket lives under ~/.tariboyd, so it stays
	// short and bindable regardless of how deep the base data dir is.
	p, err := Resolve(env(map[string]string{"TARIBOY_BASE_DIR": longBase, "HOME": "/home/u"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Base != longBase {
		t.Fatalf("Base = %q", p.Base)
	}
	if got := p.RuntimeDir(); got != "/home/u/.tariboyd" {
		t.Fatalf("Resolve RuntimeDir = %q, want /home/u/.tariboyd", got)
	}
	if err := BindableSocketPath(p.Socket()); err != nil {
		t.Fatalf("daemon control socket unbindable despite long base: %v", err)
	}

	// New(base): agent shim sockets are base-relative, so a long base relocates
	// them under ~/.tariboy/run (keyed by a hash of base) to stay bindable.
	t.Setenv("HOME", "/home/u")
	t.Setenv("TARIBOY_RUNTIME_DIR", "")
	rt := New(longBase).RuntimeDir()
	if filepath.Dir(rt) != "/home/u/.tariboy/run" {
		t.Fatalf("New RuntimeDir = %q, want under /home/u/.tariboy/run", rt)
	}
	if rt == longBase {
		t.Fatal("long base was not relocated off the data dir")
	}
	if err := BindableSocketPath(New(longBase).Socket()); err != nil {
		t.Fatalf("shim socket unbindable despite long base: %v", err)
	}
}

func TestBindableSocketPath(t *testing.T) {
	if err := BindableSocketPath("/short/a.sock"); err != nil {
		t.Fatalf("short path rejected: %v", err)
	}
	long := "/" + string(make([]byte, MaxSockPath)) + ".sock"
	if err := BindableSocketPath(long); err == nil {
		t.Fatal("over-limit path accepted")
	}
}

func TestBindableSocketPathUsesCrossPlatformLimit(t *testing.T) {
	max := "/" + strings.Repeat("a", 102)
	if err := BindableSocketPath(max); err != nil {
		t.Fatalf("103-byte path rejected: %v", err)
	}
	if err := BindableSocketPath(max + "a"); err == nil {
		t.Fatal("104-byte path accepted")
	}
}

func TestEnsureBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sa")
	p := Paths{Base: base}
	if err := p.EnsureBase(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{
		base,
		p.AgentsDir(),
		p.ImagesDir(),
		p.PluginsDir(),
		p.ImageSourcesDir(),
		p.WorkflowsDir(),
	} {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			t.Fatalf("missing dir %s: %v", d, err)
		}
		if st.Mode().Perm() != 0o700 {
			t.Fatalf("%s perm = %o, want 700", d, st.Mode().Perm())
		}
	}
}
