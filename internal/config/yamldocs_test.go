package config

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestParseYAMLDocuments is the regression test of the documents the
// parser dropped after an empty one (a document commented out, or two
// markers in a row): every document is kept, at its line in the file.
func TestParseYAMLDocuments(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string // the body of every document, "" for an empty one, with its line
	}{
		{"one document", "a: 1\n", []string{"1: a: 1"}},
		{"two documents", "a: 1\n---\nb: 2\n", []string{"1: a: 1", "3: b: 2"}},
		{"a document commented out", "a: 1\n---\n# b: 2\n---\nc: 3\n", []string{"1: a: 1", "", "5: c: 3"}},
		{"two markers in a row", "a: 1\n---\n---\nb: 2\n---\nc: 3\n", []string{"1: a: 1", "", "4: b: 2", "6: c: 3"}},
		{"a leading marker", "---\na: 1\n---\nb: 2\n", []string{"2: a: 1", "4: b: 2"}},
		{"comments before the first marker", "# header\n---\na: 1\n", []string{"3: a: 1"}},
		{"a trailing document commented out", "a: 1\n---\n# b: 2\n", []string{"1: a: 1", ""}},
		{"a marker in a block scalar", "a: |\n  ---\n  text\n---\nb: 2\n", []string{"1: a: |\n  ---\n  text", "5: b: 2"}},
		{"no document", "", []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := parseYAMLDocuments([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range docs {
				if d.Body == nil {
					got = append(got, "")
					continue
				}
				got = append(got, fmt.Sprintf("%d: %s", d.Body.GetToken().Position.Line, d.Body.String()))
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("documents (-want +got):\n%s", diff)
			}
		})
	}
}
