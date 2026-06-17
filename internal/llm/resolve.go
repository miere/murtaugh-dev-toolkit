package llm

import (
	"fmt"
	"strings"
)

// Family identifies which litellm provider family backs a Provider. Compat
// endpoints (Z.ai / DeepSeek / Kimi / GLM, etc.) ride the anthropic or openai
// family with a custom base_url; they do not get their own Family.
type Family string

const (
	// FamilyGemini maps to litellm's "gemini" provider.
	FamilyGemini Family = "gemini"
	// FamilyAnthropic maps to litellm's "anthropic" provider (and
	// anthropic-compatible endpoints via base_url).
	FamilyAnthropic Family = "anthropic"
	// FamilyOpenAI maps to litellm's "openai" provider (and OpenAI-compatible
	// endpoints via base_url).
	FamilyOpenAI Family = "openai"
)

// ParseFamily resolves a family string ("gemini" / "anthropic" / "openai",
// case-insensitive) to a Family. It is the single place that decides which
// provider names Murtaugh accepts; compat endpoints reuse anthropic/openai.
func ParseFamily(s string) (Family, error) {
	switch Family(strings.ToLower(strings.TrimSpace(s))) {
	case FamilyGemini:
		return FamilyGemini, nil
	case FamilyAnthropic:
		return FamilyAnthropic, nil
	case FamilyOpenAI:
		return FamilyOpenAI, nil
	default:
		return "", fmt.Errorf("llm: unknown provider family %q (want gemini, anthropic, or openai)", s)
	}
}

// providerName returns the litellm builtin provider name for a Family.
func (f Family) providerName() string {
	return string(f)
}

// New constructs a litellm-backed Provider for the given family/model. baseURL
// is optional and, when set, overrides the family's default endpoint (this is
// how compat providers — Z.ai/DeepSeek/Kimi on anthropic or openai — are
// reached). apiKey is the plain credential; llm never reads config or env, the
// caller resolves it first. model is carried into each Request.
//
// llm stays free of internal/config by design: callers pass plain strings.
func New(family Family, model, baseURL, apiKey string) (Provider, error) {
	switch family {
	case FamilyGemini, FamilyAnthropic, FamilyOpenAI:
	default:
		return nil, fmt.Errorf("llm: unsupported provider family %q", family)
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("llm: model is required")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("llm: api key is required for family %q", family)
	}
	return newLiteLLMProvider(family, model, baseURL, apiKey, nil)
}
