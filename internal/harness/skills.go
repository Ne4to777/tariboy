package harness

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/agentdir"
)

const SkillAdapterContractVersion = "2"

const codexSkillCatalogLimit = 8000

const codexSkillCatalogPreamble = `## Image skills

The following skills are packaged by the active Tariboy image. Match requests against each description. Before using a matching skill, read its SKILL.md completely from the exact absolute path shown. Treat the description only as selection metadata; the file is the authoritative workflow.

`

const (
	minimumClaudeSkillBridgeVersion = "2.1.227"
)

var (
	skillDescriptorName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	semanticVersion     = regexp.MustCompile(`(?:^|[^0-9])(\d+)\.(\d+)\.(\d+)(?:[^0-9]|$)`)
)

type SkillDescriptor struct {
	Name        string
	Description string
	TreeSHA256  string
}

type SkillBridgeRequest struct {
	ImageName   string
	ImageDigest string
	BridgeDir   string
	Skills      []SkillDescriptor
}

type SkillLaunchConfig struct {
	Args         []string
	Env          []string
	PromptPrefix string
}

type SkillSupportProbe struct {
	Args           []string
	Env            []string
	MinimumVersion string
	RequiredOutput string
	Contract       string
}

type SkillBridge struct {
	Plan    agentdir.BridgePlan
	Launch  SkillLaunchConfig
	Support SkillSupportProbe
}

type boundedOutput struct {
	buf       bytes.Buffer
	remaining int
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	if len(p) > 0 {
		_, _ = w.buf.Write(p)
		w.remaining -= len(p)
	}
	return n, nil
}

func normalizeGeneratedName(value string) string {
	value = strings.ToLower(value)
	var out strings.Builder
	lastHyphen := false
	for _, r := range value {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			out.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen && out.Len() > 0 {
			out.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func generatedPluginName(imageName, digest string) string {
	base := normalizeGeneratedName(imageName)
	if base == "" {
		base = "image"
	}
	suffix := "-" + digest[:8]
	prefix := "tariboy-image-"
	maxBase := 64 - len(prefix) - len(suffix)
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	return prefix + base + suffix
}

func validateSkillBridgeRequest(request SkillBridgeRequest, harness string) error {
	if len(request.ImageDigest) != 64 {
		return errors.New("image skill bridge requires a full image digest")
	}
	if _, err := hex.DecodeString(request.ImageDigest); err != nil || request.ImageDigest != strings.ToLower(request.ImageDigest) {
		return errors.New("image skill bridge has an invalid image digest")
	}
	if !filepath.IsAbs(request.BridgeDir) || filepath.Base(request.BridgeDir) != harness {
		return fmt.Errorf("%s image skill bridge requires its absolute harness directory", harness)
	}
	seen := make(map[string]bool, len(request.Skills))
	for _, skill := range request.Skills {
		if !skillDescriptorName.MatchString(skill.Name) || skill.Description == "" || len(skill.TreeSHA256) != 64 || seen[skill.Name] {
			return fmt.Errorf("invalid image skill descriptor %q", skill.Name)
		}
		seen[skill.Name] = true
	}
	return nil
}

func generatedJSON(path string, value any) (agentdir.BridgeFile, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return agentdir.BridgeFile{}, err
	}
	body = append(body, '\n')
	return agentdir.BridgeFile{Path: filepath.ToSlash(path), Body: body, Mode: 0o600}, nil
}

func claudeSkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	if len(request.Skills) == 0 {
		return SkillBridge{}, nil
	}
	if err := validateSkillBridgeRequest(request, "claude"); err != nil {
		return SkillBridge{}, err
	}
	plugin := generatedPluginName(request.ImageName, request.ImageDigest)
	manifest, err := generatedJSON(".claude-plugin/plugin.json", struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}{Name: plugin, Version: "0.0.0", Description: "Agent Skills packaged by Tariboy image " + request.ImageName})
	if err != nil {
		return SkillBridge{}, err
	}
	return SkillBridge{
		Plan:   agentdir.BridgePlan{SkillDestination: "skills", Files: []agentdir.BridgeFile{manifest}},
		Launch: SkillLaunchConfig{Args: []string{"--plugin-dir", request.BridgeDir}},
		Support: SkillSupportProbe{
			Args: []string{"--version"}, MinimumVersion: minimumClaudeSkillBridgeVersion,
		},
	}, nil
}

func codexSkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	if len(request.Skills) == 0 {
		return SkillBridge{}, nil
	}
	if err := validateSkillBridgeRequest(request, "codex"); err != nil {
		return SkillBridge{}, err
	}
	return SkillBridge{
		Plan:   agentdir.BridgePlan{SkillDestination: "skills"},
		Launch: SkillLaunchConfig{PromptPrefix: codexSkillCatalog(request)},
	}, nil
}

type codexCatalogEntry struct {
	prefix      string
	description []rune
	suffix      string
}

func codexSkillCatalog(request SkillBridgeRequest) string {
	entries := make([]codexCatalogEntry, 0, len(request.Skills))
	minimalRunes := 0
	for _, skill := range request.Skills {
		path := filepath.Join(request.BridgeDir, "skills", skill.Name, "SKILL.md")
		entry := codexCatalogEntry{
			prefix:      "- " + escapeCodexCatalogMetadata(skill.Name),
			description: []rune(escapeCodexCatalogMetadata(skill.Description)),
			suffix:      " (file: " + escapeCodexCatalogMetadata(path) + ")\n",
		}
		minimalRunes += len([]rune(entry.prefix)) + len([]rune(entry.suffix))
		entries = append(entries, entry)
	}

	preambleRunes := len([]rune(codexSkillCatalogPreamble))
	if preambleRunes+minimalRunes+1 <= codexSkillCatalogLimit {
		remaining := codexSkillCatalogLimit - preambleRunes - minimalRunes - 1
		var out strings.Builder
		out.WriteString(codexSkillCatalogPreamble)
		for _, entry := range entries {
			out.WriteString(entry.prefix)
			if len(entry.description) > 0 && remaining > 2 {
				out.WriteString(": ")
				remaining -= 2
				if len(entry.description) <= remaining {
					out.WriteString(string(entry.description))
					remaining -= len(entry.description)
				} else {
					if remaining > 1 {
						out.WriteString(string(entry.description[:remaining-1]))
					}
					out.WriteRune('…')
					remaining = 0
				}
			}
			out.WriteString(entry.suffix)
		}
		out.WriteByte('\n')
		return out.String()
	}

	best := 0
	used := preambleRunes
	for i, entry := range entries {
		used += len([]rune(entry.prefix)) + len([]rune(entry.suffix))
		warning := fmt.Sprintf("- %d image skills omitted by the 8000-character catalog limit\n\n", len(entries)-(i+1))
		if used+len([]rune(warning)) > codexSkillCatalogLimit {
			break
		}
		best = i + 1
	}
	var out strings.Builder
	out.WriteString(codexSkillCatalogPreamble)
	for _, entry := range entries[:best] {
		out.WriteString(entry.prefix)
		out.WriteString(entry.suffix)
	}
	fmt.Fprintf(&out, "- %d image skills omitted by the 8000-character catalog limit\n\n", len(entries)-best)
	return out.String()
}

func escapeCodexCatalogMetadata(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\u2028':
			out.WriteString(`\u2028`)
		case '\u2029':
			out.WriteString(`\u2029`)
		case '`', '*', '_', '[', ']', '(', ')', '#', '>':
			out.WriteByte('\\')
			out.WriteRune(r)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&out, `\u%04x`, r)
				continue
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}

func cursorSkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	if len(request.Skills) == 0 {
		return SkillBridge{}, nil
	}
	if err := validateSkillBridgeRequest(request, "cursor"); err != nil {
		return SkillBridge{}, err
	}
	return SkillBridge{
		Plan:   agentdir.BridgePlan{SkillDestination: "skills"},
		Launch: SkillLaunchConfig{PromptPrefix: codexSkillCatalog(request)},
	}, nil
}

func openCodeSkillBridge(request SkillBridgeRequest) (SkillBridge, error) {
	if len(request.Skills) == 0 {
		return SkillBridge{}, nil
	}
	if err := validateSkillBridgeRequest(request, "opencode"); err != nil {
		return SkillBridge{}, err
	}
	skillsPath := filepath.Join(request.BridgeDir, "skills")
	config, err := generatedJSON("opencode.json", struct {
		Skills struct {
			Paths []string `json:"paths"`
		} `json:"skills"`
	}{Skills: struct {
		Paths []string `json:"paths"`
	}{Paths: []string{skillsPath}}})
	if err != nil {
		return SkillBridge{}, err
	}
	env := "OPENCODE_CONFIG_DIR=" + request.BridgeDir
	return SkillBridge{
		Plan:   agentdir.BridgePlan{SkillDestination: "skills", Files: []agentdir.BridgeFile{config}},
		Launch: SkillLaunchConfig{Env: []string{env}},
		Support: SkillSupportProbe{
			Args: []string{"debug", "config"}, Env: []string{env}, RequiredOutput: skillsPath,
			Contract: "OPENCODE_CONFIG_DIR skills.paths",
		},
	}, nil
}

func parseVersion(output string) ([3]int, string, error) {
	match := semanticVersion.FindStringSubmatch(output)
	if len(match) != 4 {
		return [3]int{}, "", errors.New("version output does not contain semantic version")
	}
	var parsed [3]int
	for i := range parsed {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return [3]int{}, "", err
		}
		parsed[i] = value
	}
	return parsed, match[1] + "." + match[2] + "." + match[3], nil
}

func versionAtLeast(installed, minimum [3]int) bool {
	for i := range installed {
		if installed[i] != minimum[i] {
			return installed[i] > minimum[i]
		}
	}
	return true
}

func ValidateSkillBridgeSupport(executable, harness string, probe SkillSupportProbe) error {
	if len(probe.Args) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, probe.Args...)
	cmd.Env = append(os.Environ(), probe.Env...)
	output := &boundedOutput{remaining: 64 << 10}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s image skills require %s support: capability probe failed: %w", harness, requiredSupport(probe), err)
	}
	text := output.buf.String()
	if probe.RequiredOutput != "" {
		if !strings.Contains(text, probe.RequiredOutput) {
			return fmt.Errorf("%s image skills require %s support; installed harness did not expose the generated skill path", harness, requiredSupport(probe))
		}
		return nil
	}
	installed, installedText, err := parseVersion(text)
	if err != nil {
		return fmt.Errorf("%s image skills require version %s or newer; installed version is unrecognized", harness, probe.MinimumVersion)
	}
	minimum, _, err := parseVersion(probe.MinimumVersion)
	if err != nil {
		return fmt.Errorf("invalid %s minimum skill bridge version %q", harness, probe.MinimumVersion)
	}
	if !versionAtLeast(installed, minimum) {
		return fmt.Errorf("%s image skills require version %s or newer; installed version is %s", harness, probe.MinimumVersion, installedText)
	}
	return nil
}

func requiredSupport(probe SkillSupportProbe) string {
	if probe.Contract != "" {
		return probe.Contract
	}
	return "version " + probe.MinimumVersion + " or newer"
}

func LegacySkillsSubdir(harness string) string {
	switch harness {
	case "claude", "codex", "opencode", "cursor", "stub":
		return ".claude/skills"
	default:
		return ""
	}
}
