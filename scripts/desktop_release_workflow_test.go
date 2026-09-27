package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDesktopMacTargetRunsTheReleasePackager(t *testing.T) {
	root := repositoryRoot(t)
	cmd := exec.Command("make", "-n", "desktop-mac")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run desktop-mac: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "./scripts/package-alpha.sh") {
		t.Fatalf("desktop-mac does not run the existing release packager:\n%s", output)
	}
	legacy := exec.Command("make", "-n", "desktop-alpha")
	legacy.Dir = root
	if output, err := legacy.CombinedOutput(); err == nil {
		t.Fatalf("legacy desktop-alpha target still exists:\n%s", output)
	}
}

func TestDesktopMacPackagerRequiresSigningSecrets(t *testing.T) {
	root := repositoryRoot(t)
	baseEnv := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "TAURI_SIGNING_PRIVATE_KEY=") &&
			!strings.HasPrefix(value, "TAURI_SIGNING_PRIVATE_KEY_PASSWORD=") {
			baseEnv = append(baseEnv, value)
		}
	}
	for _, testCase := range []struct {
		name     string
		key      string
		password string
		want     string
	}{
		{name: "private key", password: "fixture", want: "TAURI_SIGNING_PRIVATE_KEY is required"},
		{name: "password", key: "fixture", want: "TAURI_SIGNING_PRIVATE_KEY_PASSWORD is required"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cmd := exec.Command("bash", filepath.Join(root, "scripts", "package-alpha.sh"))
			cmd.Dir = root
			cmd.Env = append(baseEnv,
				"TAURI_SIGNING_PRIVATE_KEY="+testCase.key,
				"TAURI_SIGNING_PRIVATE_KEY_PASSWORD="+testCase.password,
			)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), testCase.want) {
				t.Fatalf("packager did not reject missing %s (err=%v):\n%s", testCase.name, err, output)
			}
		})
	}
}

func TestDesktopReleaseWorkflowPublishesCheckedTagArtifacts(t *testing.T) {
	root := repositoryRoot(t)
	path := filepath.Join(root, ".github", "workflows", "desktop-release.yml")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Desktop release workflow: %v", err)
	}

	var workflow struct {
		On struct {
			Push struct {
				Tags []string `yaml:"tags"`
			} `yaml:"push"`
		} `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			RunsOn      string            `yaml:"runs-on"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Uses string            `yaml:"uses"`
				With map[string]any    `yaml:"with"`
				Run  string            `yaml:"run"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		t.Fatalf("parse Desktop release workflow: %v", err)
	}
	if got, want := workflow.On.Push.Tags, []string{"v[0-9]+.[0-9]+.[0-9]+"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("release tags = %v, want %v", got, want)
	}
	if len(workflow.Permissions) != 0 {
		t.Fatalf("top-level release permissions = %v, want job-scoped permissions", workflow.Permissions)
	}

	job, ok := workflow.Jobs["release"]
	if !ok {
		t.Fatal("release workflow has no release job")
	}
	if job.RunsOn != "macos-15" {
		t.Fatalf("release runner = %q, want macos-15", job.RunsOn)
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "write" {
		t.Fatalf("release job permissions = %v, want only contents: write", job.Permissions)
	}
	commands := make([]string, 0, len(job.Steps))
	var publishEnv map[string]string
	var buildEnv map[string]string
	var cargoCache bool
	for _, step := range job.Steps {
		commands = append(commands, step.Run)
		if step.Uses == "Swatinem/rust-cache@6323deb102c322ba6fcbdcafc7e3dddab59af2b6" && step.With["workspaces"] == "desktop/src-tauri -> target" {
			cargoCache = true
		}
		if strings.HasPrefix(step.Uses, "actions/checkout@") && step.With["persist-credentials"] != false {
			t.Fatalf("checkout persist-credentials = %v, want false", step.With["persist-credentials"])
		}
		if strings.Contains(step.Run, "gh release create") {
			publishEnv = step.Env
		}
		if strings.Contains(step.Run, "make desktop-mac") {
			buildEnv = step.Env
		}
	}
	allCommands := strings.Join(commands, "\n")
	for _, required := range []string{
		`GITHUB_REF_NAME#v`,
		`internal/version/version.go`,
		`scripts/release-version.txt`,
		`brew install tmux ripgrep`,
		`make desktop-mac`,
		`scripts/desktop-updater-manifest.py`,
		`desktop/src-tauri/tauri.conf.json`,
		`*.app.tar.gz`,
		`*.app.tar.gz.sig`,
		`gh release create "$GITHUB_REF_NAME"`,
		`--draft`,
		`gh release upload "$GITHUB_REF_NAME" "$release_dir"/*`,
		`gh release edit "$GITHUB_REF_NAME" --draft=false`,
	} {
		if !strings.Contains(allCommands, required) {
			t.Errorf("release workflow is missing %q", required)
		}
	}
	if publishEnv["GH_TOKEN"] != "${{ github.token }}" {
		t.Fatalf("release GH_TOKEN = %q, want github.token", publishEnv["GH_TOKEN"])
	}
	if buildEnv["TAURI_SIGNING_PRIVATE_KEY"] != "${{ secrets.TAURI_SIGNING_PRIVATE_KEY }}" ||
		buildEnv["TAURI_SIGNING_PRIVATE_KEY_PASSWORD"] != "${{ secrets.TAURI_SIGNING_PRIVATE_KEY_PASSWORD }}" {
		t.Fatalf("release signing environment = %v, want signing Secrets on build step", buildEnv)
	}
	if publishEnv["TAURI_SIGNING_PRIVATE_KEY"] != "" || publishEnv["TAURI_SIGNING_PRIVATE_KEY_PASSWORD"] != "" {
		t.Fatal("release signing Secrets are exposed to publication step")
	}
	if !cargoCache {
		t.Fatal("release workflow has no pinned Cargo cache for the desktop target")
	}
}

// A tag run restores only caches of its own tag or of main, so the release must
// restore the cache main warms and never save its own.
func TestDesktopReleaseRestoresTheCacheWarmedOnMain(t *testing.T) {
	root := repositoryRoot(t)
	type step struct {
		Uses string         `yaml:"uses"`
		With map[string]any `yaml:"with"`
		Run  string         `yaml:"run"`
	}
	var release, warm struct {
		On   map[string]any `yaml:"on"`
		Jobs map[string]struct {
			RunsOn string `yaml:"runs-on"`
			Steps  []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	for path, workflow := range map[string]any{"desktop-release.yml": &release, "desktop-cache.yml": &warm} {
		contents, err := os.ReadFile(filepath.Join(root, ".github", "workflows", path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if err := yaml.Unmarshal(contents, workflow); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
	}
	cache := func(steps []step) map[string]any {
		for _, s := range steps {
			if strings.HasPrefix(s.Uses, "Swatinem/rust-cache@") {
				return s.With
			}
		}
		t.Fatal("no Cargo cache step")
		return nil
	}

	releaseCache := cache(release.Jobs["release"].Steps)
	if releaseCache["shared-key"] != "desktop-release" || releaseCache["save-if"] != false {
		t.Fatalf("release Cargo cache = %v, want shared-key desktop-release and save-if false", releaseCache)
	}

	push, _ := warm.On["push"].(map[string]any)
	if branches, _ := push["branches"].([]any); len(branches) != 1 || branches[0] != "main" {
		t.Fatalf("cache warm push branches = %v, want [main]", push["branches"])
	}
	job, ok := warm.Jobs["warm"]
	if !ok || job.RunsOn != release.Jobs["release"].RunsOn {
		t.Fatalf("cache warm job must run on the release runner %q", release.Jobs["release"].RunsOn)
	}
	warmCache := cache(job.Steps)
	if warmCache["shared-key"] != "desktop-release" || warmCache["workspaces"] != releaseCache["workspaces"] {
		t.Fatalf("cache warm Cargo cache = %v, want the release key and workspace", warmCache)
	}
	if _, ok := warmCache["save-if"]; ok {
		t.Fatalf("cache warm Cargo cache must save, got save-if %v", warmCache["save-if"])
	}
	var commands []string
	for _, s := range job.Steps {
		commands = append(commands, s.Run)
	}
	// The cargo invocation `cargo tauri build` runs for a release.
	if all := strings.Join(commands, "\n"); !strings.Contains(all, "cargo build --bins --release --locked --features tauri/custom-protocol") {
		t.Fatalf("cache warm job does not build like cargo tauri build:\n%s", all)
	}
}
