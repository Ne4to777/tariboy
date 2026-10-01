package image

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/agentskills"
	"github.com/alekzonder/tariboy/internal/imagecontract"
	"github.com/alekzonder/tariboy/internal/imagefile"
	"github.com/alekzonder/tariboy/internal/plugincaps"
)

var archiveBaseCleaner = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

const maxV2PromptBytes = int64(32 << 20)

type preparedV2 struct {
	plugins  []ManifestPlugin
	skills   []agentskills.Prepared
	entries  []TemplateEntry
	layers   map[string][]byte
	template PromptTemplate
}

func prepareV2(src *imagefile.V2, roots imagefile.ResolveRoots, resolver plugincaps.ExternalResolver) (preparedV2, error) {
	if src == nil || src.SchemaVersion != 2 {
		return preparedV2{}, fmt.Errorf("schema-v2 source is required")
	}
	if len(src.Extends) > 0 {
		return preparedV2{}, errors.New(imagefile.ExtendsUnassembledMessage)
	}
	plugins := make([]ManifestPlugin, 0, len(src.Plugins))
	for _, plugin := range src.Plugins {
		if _, err := plugincaps.ValidateExplicit([]string{plugin.Name}, resolver); err != nil {
			return preparedV2{}, err
		}
		plugins = append(plugins, ManifestPlugin{Name: plugin.Name})
	}
	skills := make([]agentskills.Prepared, 0, len(src.Skills))
	sourceSkills := roots.SourceSkills
	if sourceSkills == nil {
		sourceSkills = make(map[string]string)
	}
	for i, skill := range src.Skills {
		resolved, err := imagefile.ResolveSkillDirectory(src.Dir, skill.Dir, roots)
		if err != nil {
			return preparedV2{}, fmt.Errorf("skills[%d]: %w", i, err)
		}
		prepared, err := agentskills.Prepare(resolved)
		if err != nil {
			return preparedV2{}, fmt.Errorf("skills[%d]: %w", i, err)
		}
		skills = append(skills, prepared)
		if roots.SourceSkills == nil && (strings.HasPrefix(skill.Dir, "./") || strings.HasPrefix(skill.Dir, "../")) {
			sourceSkills[skill.Dir] = resolved.Path
		}
	}
	if err := agentskills.ValidateSet(skills); err != nil {
		return preparedV2{}, err
	}
	contract := imagecontract.Input{Plugins: make([]string, 0, len(plugins)), Skills: make([]imagecontract.Skill, 0, len(skills))}
	for _, plugin := range plugins {
		contract.Plugins = append(contract.Plugins, plugin.Name)
	}
	for _, skill := range skills {
		executables := make(map[string]bool)
		for _, file := range skill.Files {
			if file.Executable {
				executables[file.RelativePath] = true
			}
		}
		contract.Skills = append(contract.Skills, imagecontract.Skill{Name: skill.Metadata.Name, Executables: executables})
	}
	for _, prompt := range src.Prompts {
		if prompt.Runtime != "" {
			contract.Runtimes = append(contract.Runtimes, prompt.Runtime)
		}
	}
	if err := imagecontract.Validate(contract, resolver); err != nil {
		return preparedV2{}, err
	}
	entries := make([]TemplateEntry, 0, len(src.Prompts))
	layers := map[string][]byte{}
	var totalPromptBytes int64
	promptRoots := roots
	promptRoots.SourceSkills = sourceSkills
	for i, prompt := range src.Prompts {
		if prompt.Runtime != "" {
			entries = append(entries, TemplateEntry{Kind: "runtime", Runtime: prompt.Runtime})
			continue
		}
		resolved, err := imagefile.ResolvePromptFile(src.Dir, prompt.File, promptRoots)
		if err != nil {
			return preparedV2{}, fmt.Errorf("prompts[%d]: %w", i, err)
		}
		body, err := os.ReadFile(resolved.Path)
		if err != nil {
			return preparedV2{}, err
		}
		if int64(len(body)) != resolved.Size || sha256hex(body) != resolved.SHA256 {
			return preparedV2{}, fmt.Errorf("prompts[%d]: source changed while building", i)
		}
		if totalPromptBytes > maxV2PromptBytes-resolved.Size {
			return preparedV2{}, fmt.Errorf("prompt layers exceed %d aggregate bytes", maxV2PromptBytes)
		}
		totalPromptBytes += resolved.Size
		base := archiveBaseCleaner.ReplaceAllString(filepath.Base(resolved.Path), "-")
		archivePath := fmt.Sprintf("prompt/layers/%03d-%s", i, base)
		layers[archivePath] = body
		entries = append(entries, TemplateEntry{Kind: "file", Source: resolved.Source, Category: resolved.Category, ArchivePath: archivePath, Size: resolved.Size, SHA256: resolved.SHA256})
	}
	if entries == nil {
		entries = []TemplateEntry{}
	}
	hash, err := templateHash(entries)
	if err != nil {
		return preparedV2{}, err
	}
	template := PromptTemplate{SchemaVersion: 2, Entries: entries, SHA256: hash}
	return preparedV2{plugins: plugins, skills: skills, entries: entries, layers: layers, template: template}, nil
}

type ValidationV2 struct {
	Template PromptTemplate  `json:"template"`
	Skills   []ManifestSkill `json:"skills"`
}

func manifestSkills(prepared []agentskills.Prepared) []ManifestSkill {
	out := make([]ManifestSkill, 0, len(prepared))
	for _, skill := range prepared {
		meta := skill.Metadata
		out = append(out, ManifestSkill{
			Name: meta.Name, Description: meta.Description, Source: meta.Source, Category: meta.Category,
			ClientVersion: meta.ClientVersion,
			ArchiveRoot:   meta.ArchiveRoot, FileCount: meta.FileCount, Size: meta.Size, TreeSHA256: meta.TreeSHA256,
		})
	}
	return out
}

func ValidateV2Detailed(src *imagefile.V2, roots imagefile.ResolveRoots, resolver plugincaps.ExternalResolver) (ValidationV2, error) {
	prepared, err := prepareV2(src, roots, resolver)
	if err != nil {
		return ValidationV2{}, err
	}
	return ValidationV2{Template: prepared.template, Skills: manifestSkills(prepared.skills)}, nil
}

func ValidateV2(src *imagefile.V2, roots imagefile.ResolveRoots, resolver plugincaps.ExternalResolver) (PromptTemplate, error) {
	validated, err := ValidateV2Detailed(src, roots, resolver)
	if err != nil {
		return PromptTemplate{}, err
	}
	return validated.Template, nil
}

// BuildV2 is the one schema-v2 build mechanism. Operators, agents, Store
// sources and team imports all publish through it, and it may always rebuild a
// ref that already exists. Callers that accept a user-supplied ref screen it
// with BuildRefs or IsReserved first.
func BuildV2(src *imagefile.V2, roots imagefile.ResolveRoots, ref Ref, store *Store, clock func() time.Time, resolver plugincaps.ExternalResolver) (Manifest, error) {
	return buildV2(src, roots, ref, store, clock, resolver, nil)
}

// BuildV2Archive builds through BuildV2 and also returns the published archive
// bytes, so a caller can move other tags onto the same content.
func BuildV2Archive(src *imagefile.V2, roots imagefile.ResolveRoots, ref Ref, store *Store, clock func() time.Time, resolver plugincaps.ExternalResolver) (Manifest, []byte, error) {
	var archive []byte
	manifest, err := buildV2(src, roots, ref, store, clock, resolver, &archive)
	return manifest, archive, err
}

func buildV2(src *imagefile.V2, roots imagefile.ResolveRoots, ref Ref, store *Store, clock func() time.Time, resolver plugincaps.ExternalResolver, archiveOut *[]byte) (Manifest, error) {
	prepared, err := prepareV2(src, roots, resolver)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{SchemaVersion: 2, ImageVersion: src.ImageVersion, ID: RefID(ref.Name, src.ImageVersion), Name: ref.Name, Tag: ref.Tag, BuiltAt: clock().UTC().Format(time.RFC3339), Plugins: prepared.plugins, Skills: manifestSkills(prepared.skills), PromptTemplateSHA256: prepared.template.SHA256}
	if manifest.Plugins == nil {
		manifest.Plugins = []ManifestPlugin{}
	}
	if manifest.Skills == nil {
		manifest.Skills = []ManifestSkill{}
	}
	digest, err := store.writeV2Archive(ref, manifest, prepared.template, prepared.layers, prepared.skills, archiveOut)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Digest = digest
	return manifest, nil
}

func (s *Store) writeV2Archive(ref Ref, man Manifest, template PromptTemplate, layers map[string][]byte, skills []agentskills.Prepared, archiveOut *[]byte) (string, error) {
	if err := os.MkdirAll(s.nameDir(ref.Name), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.nameDir(ref.Name), "."+ref.Tag+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	manifestJSON, err := json.MarshalIndent(struct {
		SchemaVersion        int              `json:"schema_version"`
		ImageVersion         string           `json:"image_version,omitempty"`
		ID                   string           `json:"id,omitempty"`
		Name                 string           `json:"name"`
		BuiltAt              string           `json:"built_at"`
		Plugins              []ManifestPlugin `json:"plugins"`
		Skills               []ManifestSkill  `json:"skills"`
		PromptTemplateSHA256 string           `json:"prompt_template_sha256"`
	}{man.SchemaVersion, man.ImageVersion, man.ID, man.Name, man.BuiltAt, man.Plugins, man.Skills, man.PromptTemplateSHA256}, "", "  ")
	if err != nil {
		tmp.Close()
		return "", err
	}
	templateJSON, err := json.MarshalIndent(template, "", "  ")
	if err != nil {
		tmp.Close()
		return "", err
	}
	if err := writeTarFile(tw, "manifest.json", manifestJSON); err != nil {
		tmp.Close()
		return "", err
	}
	if err := writeTarFile(tw, "prompt/template.json", templateJSON); err != nil {
		tmp.Close()
		return "", err
	}
	for _, entry := range template.Entries {
		if entry.Kind != "file" {
			continue
		}
		if err := writeTarFile(tw, entry.ArchivePath, layers[entry.ArchivePath]); err != nil {
			tmp.Close()
			return "", err
		}
	}
	for _, skill := range skills {
		for _, file := range skill.Files {
			name := skill.Metadata.ArchiveRoot + "/" + file.RelativePath
			mode := int64(0o600)
			if file.Executable {
				mode = 0o700
			}
			if err := writeTarFileMode(tw, name, file.Body, mode); err != nil {
				tmp.Close()
				return "", err
			}
		}
	}
	if err := tw.Close(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := gz.Close(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return s.publishArchive(ref, tmpName, archiveOut)
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
