package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shared baseline flow documents reused across parser tests. Unknown-field
// variants are derived from these so the canonical encodings live in one place.
const (
	baselineJSON = `{"kind":"taskpilot","version":1,"entry":"main","tasks":{}}`
	baselineYAML = "kind: taskpilot\nversion: 1\nentry: main\ntasks: {}\n"
)

func TestLoadFile_RejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name           string
		fileName       string
		content        string
		errContains    string
		prefixContains string
	}{
		{
			name:           "json unknown field",
			fileName:       "flow.json",
			content:        strings.TrimSuffix(baselineJSON, "}") + `,"oops":true}`,
			errContains:    `unknown field "oops"`,
			prefixContains: "parse json:",
		},
		{
			name:           "yaml unknown field",
			fileName:       "flow.yaml",
			content:        baselineYAML + "oops: true\n",
			errContains:    "field oops not found",
			prefixContains: "parse yaml:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tt.fileName)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write test file: %v", err)
			}

			_, err := LoadFile(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.prefixContains) {
				t.Fatalf("expected error to contain %q, got %q", tt.prefixContains, err)
			}
			if !strings.Contains(err.Error(), tt.errContains) {
				t.Fatalf("expected error to contain %q, got %q", tt.errContains, err)
			}
		})
	}
}

func TestLoadFile_AcceptsKnownFields(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		content  string
	}{
		{
			name:     "json",
			fileName: "flow.json",
			content:  baselineJSON,
		},
		{
			name:     "yaml",
			fileName: "flow.yaml",
			content:  baselineYAML,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tt.fileName)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write test file: %v", err)
			}

			if _, err := LoadFile(path); err != nil {
				t.Fatalf("expected parse success, got %v", err)
			}
		})
	}
}
