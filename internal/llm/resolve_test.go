package llm

import "testing"

func TestParseFamily(t *testing.T) {
	tests := []struct {
		in      string
		want    Family
		wantErr bool
	}{
		{"gemini", FamilyGemini, false},
		{"anthropic", FamilyAnthropic, false},
		{"openai", FamilyOpenAI, false},
		{"GEMINI", FamilyGemini, false},
		{"  OpenAI  ", FamilyOpenAI, false},
		{"Anthropic", FamilyAnthropic, false},
		{"", "", true},
		{"vertex", "", true},
		{"claude", "", true},
	}
	for _, tt := range tests {
		got, err := ParseFamily(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseFamily(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseFamily(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseFamily(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		family  Family
		model   string
		apiKey  string
		wantErr bool
	}{
		{"ok gemini", FamilyGemini, "gemini-2.5-pro", "key", false},
		{"ok anthropic", FamilyAnthropic, "claude-x", "key", false},
		{"ok openai", FamilyOpenAI, "gpt-x", "key", false},
		{"bad family", Family("vertex"), "m", "key", true},
		{"missing model", FamilyOpenAI, "", "key", true},
		{"missing key", FamilyOpenAI, "m", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(tt.family, tt.model, "", tt.apiKey)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("New() = %v, want error", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(): unexpected error %v", err)
			}
			if p == nil {
				t.Fatal("New() returned nil provider")
			}
		})
	}
}

func TestNew_BaseURLOverride(t *testing.T) {
	// A compat endpoint (e.g. Z.ai on the openai family) must accept a custom
	// base_url without error.
	p, err := New(FamilyOpenAI, "glm-4", "https://api.z.ai/v1", "key")
	if err != nil {
		t.Fatalf("New() with base_url override: %v", err)
	}
	lp, ok := p.(*litellmProvider)
	if !ok {
		t.Fatalf("New() returned %T, want *litellmProvider", p)
	}
	if lp.family != FamilyOpenAI || lp.model != "glm-4" {
		t.Errorf("provider = %+v", lp)
	}
}
