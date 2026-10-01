// Package plugincaps declares built-in capability metadata.
package plugincaps

import (
	"fmt"

	"github.com/alekzonder/tariboy/internal/imagecontract"
)

var (
	CORE = []string{"whoami", "loop", "messages"}
	// OPTIONAL contains built-in capabilities selectable in an image. External
	// capabilities are accepted only through a resolver for installed manifests.
	OPTIONAL = []string{"context", "status", "schedule", "scripts", "goal", "image-creator", "tasks"}
	// INSTRUCTION_ONLY contains schema-v2 built-ins that contribute no route,
	// command, or shim.
	INSTRUCTION_ONLY = []string{"workdir"}
)

// ResolvedPlugin is the capability information resolved for an installed
// external plugin.
type ResolvedPlugin = imagecontract.ResolvedPlugin
type ExternalResolver = imagecontract.ExternalResolver

func known(name string) bool {
	for _, n := range CORE {
		if n == name {
			return true
		}
	}
	return IsOptional(name)
}

func knownExplicit(name string) bool {
	_, ok := imagecontract.Builtin(name)
	return ok
}

func IsOptional(name string) bool {
	for _, n := range OPTIONAL {
		if n == name {
			return true
		}
	}
	return false
}

// ResolveWithExternal returns the core plugins followed by requested ones,
// the plugin union already-built schema-v1 manifests run with.
func ResolveWithExternal(requested []string, resolver ExternalResolver) ([]string, error) {
	out := make([]string, 0, len(CORE)+len(requested))
	seen := map[string]bool{}
	for _, n := range CORE {
		out = append(out, n)
		seen[n] = true
	}
	for _, n := range requested {
		if !known(n) {
			if resolver == nil {
				return nil, fmt.Errorf("unknown plugin %q", n)
			}
			external, err := resolver(n)
			if err != nil {
				return nil, err
			}
			if !external.Installed {
				return nil, fmt.Errorf("unknown plugin %q", n)
			}
		}
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out, nil
}

// ValidateExplicit validates schema-v2 plugins without adding or reordering.
func ValidateExplicit(requested []string, resolver ExternalResolver) ([]string, error) {
	out := make([]string, 0, len(requested))
	seen := map[string]bool{}
	for _, name := range requested {
		if seen[name] {
			return nil, fmt.Errorf("duplicate plugin %q", name)
		}
		seen[name] = true
		if !knownExplicit(name) {
			if resolver == nil {
				return nil, fmt.Errorf("unknown plugin %q", name)
			}
			external, err := resolver(name)
			if err != nil {
				return nil, err
			}
			if !external.Installed {
				return nil, fmt.Errorf("unknown plugin %q", name)
			}
		}
		out = append(out, name)
	}
	return out, nil
}
