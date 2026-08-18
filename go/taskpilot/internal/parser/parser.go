package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"gopkg.in/yaml.v3"
)

// Format identifies the on-disk encoding of a workflow file, selected from its
// filename extension.
type Format int

const (
	// FormatUnknown means the extension is not a recognized workflow format.
	FormatUnknown Format = iota
	// FormatYAML is selected for .yaml and .yml files.
	FormatYAML
	// FormatJSON is selected for .json files.
	FormatJSON
)

// FormatForPath returns the workflow format implied by path's filename
// extension. The recognized extensions are .yaml and .yml (YAML) and .json
// (JSON); any other extension yields FormatUnknown. LoadFile and CLI
// autodetection share this extension resolution.
func FormatForPath(path string) Format {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return FormatYAML
	case ".json":
		return FormatJSON
	default:
		return FormatUnknown
	}
}

// LoadFile reads the file at path and parses it into a model.Document. The
// format is selected from the extension via FormatForPath: .yaml and .yml are
// decoded as YAML, .json as JSON, and any other extension is rejected. Both
// formats reject unknown fields and require exactly one document/top-level
// value. It returns an error if the file cannot be read or if parsing fails.
func LoadFile(path string) (*model.Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc model.Document
	switch FormatForPath(path) {
	case FormatYAML:
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("parse yaml: %w", err)
		}
		// Reject extra YAML documents to enforce a single strict contract document.
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("parse yaml: multiple documents are not allowed")
			}
			return nil, fmt.Errorf("parse yaml: %w", err)
		}
	case FormatJSON:
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("parse json: %w", err)
		}
		// Reject trailing JSON values to enforce a single strict contract document.
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("parse json: multiple top-level values are not allowed")
			}
			return nil, fmt.Errorf("parse json: %w", err)
		}
	default:
		return nil, fmt.Errorf("unrecognized workflow extension %q: want .yaml, .yml, or .json", filepath.Ext(path))
	}
	return &doc, nil
}
