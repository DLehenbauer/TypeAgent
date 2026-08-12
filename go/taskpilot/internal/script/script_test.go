package script_test

import (
	"testing"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/model"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/script"
)

func TestDecodeRendersFileReferences(t *testing.T) {
	req, err := script.Decode(map[string]any{
		"script": "Write-Output ok",
		"args":   []any{"literal", model.FileRef(`C:\data.txt`, "abc"), 42},
	}, model.FileRefPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"literal", `C:\data.txt`, "42"}
	for i := range want {
		if req.Args[i] != want[i] {
			t.Fatalf("args=%v want=%v", req.Args, want)
		}
	}
}

func TestDecodeRejectsMalformedInput(t *testing.T) {
	for name, input := range map[string]map[string]any{
		"missing script": {},
		"scalar args":    {"script": "x", "args": "one"},
		"bad timeout":    {"script": "x", "timeoutSeconds": 1.5},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := script.Decode(input, model.FileRefPath); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestResultMapPreservesContract(t *testing.T) {
	result, err := script.NewResult(`{"ok":true}`, "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	out := result.Map()
	if out[script.OutputExitCode] != 0 {
		t.Fatalf("output=%v", out)
	}
	value := out[script.OutputResult].(map[string]any)
	if value["ok"] != true {
		t.Fatalf("result=%v", value)
	}
}
