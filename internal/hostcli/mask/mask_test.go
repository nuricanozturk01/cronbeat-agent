package mask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type vector struct {
	Name     string `json:"name"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "mask_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors []vector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 50 {
		t.Fatalf("only %d vectors, the shared file looks truncated", len(vectors))
	}
	return vectors
}

func TestTextMatchesTheJavaSecretMaskerVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			if got := Text(v.Input); got != v.Expected {
				t.Fatalf("Text(%q)\n got  %q\n want %q", v.Input, got, v.Expected)
			}
		})
	}
}

func TestVectorNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range loadVectors(t) {
		if seen[v.Name] {
			t.Fatalf("duplicate vector name %q", v.Name)
		}
		seen[v.Name] = true
	}
}

func TestTextHandlesLargeInputQuickly(t *testing.T) {
	in := strings.Repeat("token=abc user@example.com plain text ", 2000)
	out := Text(in)
	if strings.Contains(out, "user@example.com") || strings.Contains(out, "=abc") {
		t.Fatal("large input was not masked")
	}
}
