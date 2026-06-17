package main

import (
	"reflect"
	"testing"
)

func TestExtractJSONFlag(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantEnabled bool
		wantRest    []string
	}{
		{
			name:        "absent",
			args:        []string{"ping"},
			wantEnabled: false,
			wantRest:    []string{"ping"},
		},
		{
			name:        "leading bare flag",
			args:        []string{"--json", "ping"},
			wantEnabled: true,
			wantRest:    []string{"ping"},
		},
		{
			name:        "trailing bare flag",
			args:        []string{"ping", "--json"},
			wantEnabled: true,
			wantRest:    []string{"ping"},
		},
		{
			name:        "explicit true",
			args:        []string{"--json=true", "ping"},
			wantEnabled: true,
			wantRest:    []string{"ping"},
		},
		{
			name:        "explicit false",
			args:        []string{"--json=false", "ping"},
			wantEnabled: false,
			wantRest:    []string{"ping"},
		},
		{
			name:        "preserves other flags",
			args:        []string{"slack", "send-msg", "--to", "C1", "--json"},
			wantEnabled: true,
			wantRest:    []string{"slack", "send-msg", "--to", "C1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enabled, rest, err := extractJSONFlag(tt.args)
			if err != nil {
				t.Fatalf("extractJSONFlag returned error: %v", err)
			}
			if enabled != tt.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tt.wantEnabled)
			}
			if !reflect.DeepEqual(rest, tt.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

func TestExtractJSONFlag_InvalidValue(t *testing.T) {
	if _, _, err := extractJSONFlag([]string{"--json=maybe", "ping"}); err == nil {
		t.Fatal("extractJSONFlag returned nil error, want error for invalid bool")
	}
}
