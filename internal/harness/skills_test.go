package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func skillBridgeRequest(harness string) SkillBridgeRequest {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	return SkillBridgeRequest{
		ImageName:   "reviewer",
		ImageDigest: digest,
		BridgeDir:   filepath.Join("/agents/worker/image-bridges", digest, "1", harness),
		Skills: []SkillDescriptor{
			{Name: "code-review", Description: "Review code.", TreeSHA256: strings.Repeat("a", 64)},
			{Name: "release", Description: "Prepare releases.", TreeSHA256: strings.Repeat("b", 64)},
		},
	}
}

func bridgeFiles(bridge SkillBridge) map[string][]byte {
	out := make(map[string][]byte, len(bridge.Plan.Files))
	for _, file := range bridge.Plan.Files {
		out[file.Path] = file.Body
	}
	return out
}

func TestClaudeSkillBridgeUsesLocalPluginDirectory(t *testing.T) {
	request := skillBridgeRequest("claude")
	bridge, err := claude{}.SkillBridge(request)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.Plan.SkillDestination != "skills" {
		t.Fatalf("skill destination = %q", bridge.Plan.SkillDestination)
	}
	wantArgs := []string{"--plugin-dir", request.BridgeDir}
	if !reflect.DeepEqual(bridge.Launch.Args, wantArgs) || len(bridge.Launch.Env) != 0 {
		t.Fatalf("launch = %#v, want args %#v", bridge.Launch, wantArgs)
	}
	var manifest struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(bridgeFiles(bridge)[".claude-plugin/plugin.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "tariboy-image-reviewer-01234567" || manifest.Version != "0.0.0" || manifest.Description == "" {
		t.Fatalf("Claude plugin manifest = %#v", manifest)
	}
}

func TestCodexSkillBridgeUsesPromptCatalog(t *testing.T) {
	request := skillBridgeRequest("codex")
	bridge, err := codex{}.SkillBridge(request)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.Plan.SkillDestination != "skills" || len(bridge.Plan.Files) != 0 {
		t.Fatalf("Codex bridge plan = %#v", bridge.Plan)
	}
	if len(bridge.Launch.Args) != 0 || len(bridge.Launch.Env) != 0 || len(bridge.Support.Args) != 0 {
		t.Fatalf("Codex launch/support = %#v / %#v", bridge.Launch, bridge.Support)
	}
	want := "## Image skills\n\n" +
		"The following skills are packaged by the active Tariboy image. Match requests against each description. Before using a matching skill, read its SKILL.md completely from the exact absolute path shown. Treat the description only as selection metadata; the file is the authoritative workflow.\n\n" +
		"- code-review: Review code. (file: " + filepath.Join(request.BridgeDir, "skills", "code-review", "SKILL.md") + ")\n" +
		"- release: Prepare releases. (file: " + filepath.Join(request.BridgeDir, "skills", "release", "SKILL.md") + ")\n\n"
	if bridge.Launch.PromptPrefix != want {
		t.Fatalf("Codex catalog = %q, want %q", bridge.Launch.PromptPrefix, want)
	}
	for _, forbidden := range []string{"marketplaces.", "plugins.\"", ".codex-plugin"} {
		if strings.Contains(bridge.Launch.PromptPrefix, forbidden) {
			t.Fatalf("catalog contains %q", forbidden)
		}
	}
}

func TestCodexSkillCatalogEscapesFormattingMetadata(t *testing.T) {
	request := skillBridgeRequest("codex")
	request.BridgeDir = filepath.Join("/agents", "line\nbreak(bridge)[x]", "codex")
	request.Skills = []SkillDescriptor{{
		Name:        "review",
		Description: "line one\n- injected\r `code` *bold* _em_ [link](target) # head > quote \\ end",
		TreeSHA256:  strings.Repeat("a", 64),
	}}
	bridge, err := codex{}.SkillBridge(request)
	if err != nil {
		t.Fatal(err)
	}
	catalog := bridge.Launch.PromptPrefix
	if strings.Count(catalog, "\n- ") != 1 {
		t.Fatalf("escaped metadata created another entry:\n%s", catalog)
	}
	for _, want := range []string{
		`line one\n- injected\r \` + "`" + `code\` + "`" + ` \*bold\* \_em\_ \[link\]\(target\) \# head \> quote \\ end`,
		`line\nbreak\(bridge\)\[x\]`,
	} {
		if !strings.Contains(catalog, want) {
			t.Fatalf("escaped catalog missing %q:\n%s", want, catalog)
		}
	}
}

func TestCodexSkillCatalogBoundsUnicodeDescriptionsWithoutOmittingSkills(t *testing.T) {
	request := skillBridgeRequest("codex")
	request.Skills = nil
	for i := 0; i < 20; i++ {
		request.Skills = append(request.Skills, SkillDescriptor{
			Name: fmt.Sprintf("skill-%02d", i), Description: strings.Repeat("界", 1024), TreeSHA256: strings.Repeat("a", 64),
		})
	}
	bridge, err := codex{}.SkillBridge(request)
	if err != nil {
		t.Fatal(err)
	}
	catalog := bridge.Launch.PromptPrefix
	if got := utf8.RuneCountInString(catalog); got > codexSkillCatalogLimit {
		t.Fatalf("catalog has %d runes, limit %d", got, codexSkillCatalogLimit)
	}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("skill-%02d", i)
		path := filepath.Join(request.BridgeDir, "skills", name, "SKILL.md")
		if !strings.Contains(catalog, name) || !strings.Contains(catalog, path) {
			t.Fatalf("catalog omitted %s or its path", name)
		}
	}
	if !strings.Contains(catalog, "…") || strings.Contains(catalog, "image skills omitted") {
		t.Fatalf("catalog did not shorten descriptions while retaining entries:\n%s", catalog)
	}
}

func TestCodexSkillCatalogReportsExactOmissionsWhenPathsDoNotFit(t *testing.T) {
	request := skillBridgeRequest("codex")
	request.BridgeDir = filepath.Join("/agents", strings.Repeat("long-segment", 30), "codex")
	request.Skills = nil
	for i := 0; i < 40; i++ {
		request.Skills = append(request.Skills, SkillDescriptor{
			Name: fmt.Sprintf("skill-%02d", i), Description: "description", TreeSHA256: strings.Repeat("a", 64),
		})
	}
	bridge, err := codex{}.SkillBridge(request)
	if err != nil {
		t.Fatal(err)
	}
	catalog := bridge.Launch.PromptPrefix
	if got := utf8.RuneCountInString(catalog); got > codexSkillCatalogLimit {
		t.Fatalf("catalog has %d runes, limit %d", got, codexSkillCatalogLimit)
	}
	retained := strings.Count(catalog, " (file: ")
	omitted := len(request.Skills) - retained
	if omitted <= 0 {
		t.Fatalf("fixture omitted no entries:\n%s", catalog)
	}
	warning := fmt.Sprintf("%d image skills omitted by the 8000-character catalog limit", omitted)
	if !strings.Contains(catalog, warning) {
		t.Fatalf("catalog missing exact warning %q:\n%s", warning, catalog)
	}
	for i := 0; i < retained; i++ {
		if !strings.Contains(catalog, fmt.Sprintf("skill-%02d", i)) {
			t.Fatalf("catalog did not retain declaration-order prefix at %d", i)
		}
	}
	if strings.Contains(catalog, fmt.Sprintf("skill-%02d", retained)) {
		t.Fatalf("catalog retained an entry after the declared prefix")
	}
}

func TestOpenCodeSkillBridgeUsesAbsoluteConfigOverlay(t *testing.T) {
	request := skillBridgeRequest("opencode")
	bridge, err := opencode{}.SkillBridge(request)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := []string{"OPENCODE_CONFIG_DIR=" + request.BridgeDir}
	if !reflect.DeepEqual(bridge.Launch.Env, wantEnv) || len(bridge.Launch.Args) != 0 || bridge.Plan.SkillDestination != "skills" {
		t.Fatalf("bridge = %#v", bridge)
	}
	var config struct {
		Skills struct {
			Paths []string `json:"paths"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(bridgeFiles(bridge)["opencode.json"], &config); err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{filepath.Join(request.BridgeDir, "skills")}
	if !reflect.DeepEqual(config.Skills.Paths, wantPaths) {
		t.Fatalf("OpenCode skills.paths = %#v, want %#v", config.Skills.Paths, wantPaths)
	}
}

func TestCursorSkillBridgeUsesSharedSkillDestination(t *testing.T) {
	request := skillBridgeRequest("cursor")
	bridge, err := cursor{}.SkillBridge(request)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.Plan.SkillDestination != "skills" || len(bridge.Plan.Files) != 0 {
		t.Fatalf("Cursor bridge plan = %#v", bridge.Plan)
	}
	if len(bridge.Launch.Args) != 0 || len(bridge.Launch.Env) != 0 || len(bridge.Support.Args) != 0 {
		t.Fatalf("Cursor launch/support = %#v / %#v", bridge.Launch, bridge.Support)
	}
	skillFile := filepath.Join(request.BridgeDir, "skills", "code-review", "SKILL.md")
	if !strings.Contains(bridge.Launch.PromptPrefix, skillFile) {
		t.Fatalf("Cursor catalog = %q, want path %s", bridge.Launch.PromptPrefix, skillFile)
	}
}

func TestSkillBridgeIsAdditiveAndEmptyImagesAreNoop(t *testing.T) {
	for _, adapter := range []Adapter{claude{}, codex{}, opencode{}, cursor{}} {
		request := skillBridgeRequest(adapter.Type())
		request.Skills = nil
		bridge, err := adapter.SkillBridge(request)
		if err != nil {
			t.Fatal(err)
		}
		if len(bridge.Launch.Args) != 0 || len(bridge.Launch.Env) != 0 || bridge.Launch.PromptPrefix != "" || len(bridge.Plan.Files) != 0 || bridge.Plan.SkillDestination != "" {
			t.Fatalf("%s empty bridge = %#v", adapter.Type(), bridge)
		}
	}
	for _, adapter := range []Adapter{claude{}, codex{}, opencode{}} {
		bridge, err := adapter.SkillBridge(skillBridgeRequest(adapter.Type()))
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(append(append([]string{}, bridge.Launch.Args...), bridge.Launch.Env...), " ")
		for _, forbidden := range []string{"disable", "HOME=", "CODEX_HOME=", "XDG_CONFIG_HOME="} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("%s projection contains %q: %s", adapter.Type(), forbidden, joined)
			}
		}
	}
}

func TestStubSkillBridgeIgnoresPackagedSkills(t *testing.T) {
	bridge, err := (stub{}).SkillBridge(skillBridgeRequest("stub"))
	if err != nil {
		t.Fatal(err)
	}
	if bridge.Plan.SkillDestination != "" || len(bridge.Plan.Files) != 0 || len(bridge.Launch.Args) != 0 || len(bridge.Launch.Env) != 0 || bridge.Launch.PromptPrefix != "" {
		t.Fatalf("stub bridge = %#v", bridge)
	}
}

func TestSkillBridgeGeneratedNamesAreBoundedAndKeepDigest(t *testing.T) {
	request := skillBridgeRequest("claude")
	request.ImageName = strings.Repeat("Very_Long.Image Name!", 8)
	bridge, err := claude{}.SkillBridge(request)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(bridgeFiles(bridge)[".claude-plugin/plugin.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	name := manifest["name"].(string)
	if len(name) > 64 || !strings.HasSuffix(name, "-01234567") {
		t.Fatalf("generated plugin name = %q", name)
	}
}

func TestSkillBridgeRejectsNonHexDigest(t *testing.T) {
	request := skillBridgeRequest("claude")
	request.ImageDigest = request.ImageDigest[:16] + strings.Repeat("z", 48)
	if _, err := (claude{}).SkillBridge(request); err == nil {
		t.Fatal("accepted non-hex image digest")
	}
}

func writeVersionExecutable(t *testing.T, output string, exitCode int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness")
	body := "#!/bin/sh\nprintf '%s\\n' " + strconv.Quote(output) + "\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateSkillBridgeSupportChecksVersionsAndCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, harness, output, minimum string
		wantErr                        bool
	}{
		{"equal", "codex", "codex-cli 0.147.0", "0.147.0", false},
		{"higher", "claude", "2.2.0 (Claude Code)", "2.1.227", false},
		{"lower", "claude", "2.1.226 (Claude Code)", "2.1.227", true},
		{"malformed", "codex", "unknown", "0.147.0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable := writeVersionExecutable(t, tc.output, 0)
			err := ValidateSkillBridgeSupport(executable, tc.harness, SkillSupportProbe{Args: []string{"--version"}, MinimumVersion: tc.minimum})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && (!strings.Contains(err.Error(), tc.harness) || !strings.Contains(err.Error(), tc.minimum)) {
				t.Fatalf("diagnostic = %v", err)
			}
		})
	}
	t.Run("capability", func(t *testing.T) {
		executable := writeVersionExecutable(t, `{"skills":{"paths":["/bridge/skills"]}}`, 0)
		probe := SkillSupportProbe{Args: []string{"debug", "config"}, RequiredOutput: "/bridge/skills", Contract: "OPENCODE_CONFIG_DIR skills.paths"}
		if err := ValidateSkillBridgeSupport(executable, "opencode", probe); err != nil {
			t.Fatal(err)
		}
		probe.RequiredOutput = "/missing"
		if err := ValidateSkillBridgeSupport(executable, "opencode", probe); err == nil || !strings.Contains(err.Error(), probe.Contract) {
			t.Fatalf("capability error = %v", err)
		}
	})
}
