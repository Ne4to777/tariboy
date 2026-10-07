package aiproxy

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/alekzonder/tariboy/internal/store"
)

func TestRouterDefaultsAndRules(t *testing.T) {
	r := NewRouter()
	if u := r.Resolve("anthropic", "claude-opus-4-8"); u.BaseURL != "https://api.anthropic.com" || u.KeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("anthropic default = %+v", u)
	}
	if u := r.Resolve("openai", "gpt-4o"); u.KeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("openai default = %+v", u)
	}
	if u := r.Resolve("chatgpt", "gpt-5.6-terra"); u.BaseURL != "https://chatgpt.com/backend-api/codex" || u.KeyEnv != "" {
		t.Fatalf("chatgpt default = %+v", u)
	}
	if u := r.ResolveDefault("cursor"); u.BaseURL != "https://api2.cursor.sh" || u.KeyEnv != "" {
		t.Fatalf("cursor default = %+v", u)
	}
	// A model-glob rule overrides.
	r.SetRules([]Rule{{ModelGlob: "internal-*", Upstream: Upstream{BaseURL: "http://gw", KeyEnv: "GW_KEY"}}})
	if u := r.Resolve("anthropic", "internal-fast"); u.BaseURL != "http://gw" {
		t.Fatalf("rule override = %+v", u)
	}
	if u := r.ResolveDefault("chatgpt"); u.BaseURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("provider default must bypass model rules: %+v", u)
	}
	if u := r.Resolve("anthropic", "claude-opus-4-8"); u.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("non-matching model must keep default: %+v", u)
	}
}

func TestLoadRouterEnvOverride(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	getenv := func(k string) string {
		if k == "TARIBOY_UPSTREAM_ANTHROPIC_BASE_URL" {
			return "http://fake-upstream:9"
		}
		if k == "TARIBOY_UPSTREAM_CHATGPT_BASE_URL" {
			return "http://fake-chatgpt:10"
		}
		return ""
	}
	r, err := LoadRouter(s, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if u := r.Resolve("anthropic", "claude-opus-4-8"); u.BaseURL != "http://fake-upstream:9" {
		t.Fatalf("env override not applied: %+v", u)
	}
	if u := r.Resolve("chatgpt", ""); u.BaseURL != "http://fake-chatgpt:10" || u.KeyEnv != "" {
		t.Fatalf("chatgpt env override not applied: %+v", u)
	}
	if u := r.ResolveDefault("chatgpt"); u.BaseURL != "http://fake-chatgpt:10" || u.KeyEnv != "" {
		t.Fatalf("chatgpt default env override not applied: %+v", u)
	}
}

func TestLoadRouterChatGPTEnvOverrideCannotInheritWildcardRuleCredential(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	configured := proxyConfig{
		Upstreams: map[string]Upstream{
			"chatgpt": {BaseURL: "http://configured-chatgpt", KeyEnv: ""},
		},
		Rules: []Rule{{
			ModelGlob: "*",
			Upstream:  Upstream{BaseURL: "http://generic-rule", KeyEnv: "GENERIC_RULE_KEY"},
		}},
	}
	b, err := json.Marshal(configured)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigSet("proxy", string(b)); err != nil {
		t.Fatal(err)
	}

	r, err := LoadRouter(s, func(key string) string {
		if key == "TARIBOY_UPSTREAM_CHATGPT_BASE_URL" {
			return "http://env-chatgpt"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if u := r.ResolveDefault("chatgpt"); u.BaseURL != "http://env-chatgpt" || u.KeyEnv != "" {
		t.Fatalf("chatgpt env default contaminated by generic rule: base_ok=%t key_env_empty=%t",
			u.BaseURL == "http://env-chatgpt", u.KeyEnv == "")
	}
	if u := r.Resolve("anthropic", "claude-opus-4-8"); u.BaseURL != "http://generic-rule" || u.KeyEnv != "GENERIC_RULE_KEY" {
		t.Fatalf("anthropic generic rule changed: %+v", u)
	}
	if u := r.Resolve("openai", "gpt-4o"); u.BaseURL != "http://generic-rule" || u.KeyEnv != "GENERIC_RULE_KEY" {
		t.Fatalf("openai generic rule changed: %+v", u)
	}
}
