package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateForFilePreservesPropertyOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ext     string
		content string
	}{
		{
			name: "json",
			ext:  ".json",
			content: `{
  "openapi": "3.0.0",
  "components": {
    "schemas": {
      "Widget": {
        "type": "object",
        "properties": {
          "zeta": {"type": "string"},
          "alpha": {"type": "integer"},
          "beta": {"type": "boolean"}
        }
      }
    }
  }
}`,
		},
		{
			name: "yaml",
			ext:  ".yaml",
			content: `openapi: 3.0.0
components:
  schemas:
    Widget:
      type: object
      properties:
        zeta:
          type: string
        alpha:
          type: integer
        beta:
          type: boolean
`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tempDir := t.TempDir()
			inFile := filepath.Join(tempDir, "openapi"+tt.ext)
			outFile := filepath.Join(tempDir, "api.proto")

			if err := os.WriteFile(inFile, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("write input: %v", err)
			}

			if err := generateForFile(inFile, outFile, "api.v1", "example.com/project/api/v1;v1", true, "oneof", true, false, "value"); err != nil {
				t.Fatalf("generate proto: %v", err)
			}

			data, err := os.ReadFile(outFile)
			if err != nil {
				t.Fatalf("read output: %v", err)
			}

			content := string(data)
			assertAppearsInOrder(t, content,
				"string zeta = 1;",
				"int64 alpha = 2;",
				"bool beta = 3;",
			)
		})
	}
}

func assertAppearsInOrder(t *testing.T, content string, snippets ...string) {
	t.Helper()

	lastIndex := -1
	for _, snippet := range snippets {
		idx := strings.Index(content, snippet)
		if idx == -1 {
			t.Fatalf("missing snippet %q in output:\n%s", snippet, content)
		}
		if idx <= lastIndex {
			t.Fatalf("snippet %q appeared out of order in output:\n%s", snippet, content)
		}
		lastIndex = idx
	}
}
