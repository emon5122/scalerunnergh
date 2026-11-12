package config

import (
	"path/filepath"
	"testing"
)

// Every example config in the repo must stay valid.
func TestExamplesParse(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.json")
	if err != nil || len(files) < 4 {
		t.Fatalf("examples not found: %v %v", files, err)
	}
	for _, f := range files {
		if _, err := LoadFile(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
