package scoring

import "testing"

func TestNormalizeWhitespace(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"func foo() {", "funcfoo(){"},
		{"return   x + y", "returnx+y"},
		{"  hello  world  ", "helloworld"},
		{"", ""},
		{"nospaces", "nospaces"},
	}
	for _, tt := range tests {
		got := NormalizeWhitespace(tt.in)
		if got != tt.want {
			t.Errorf("NormalizeWhitespace(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildNormalizedSet(t *testing.T) {
	aiLines := map[string]map[string]struct{}{
		"main.go": {"func foo() {": {}, "return x + y": {}},
	}
	norm := BuildNormalizedSet(aiLines)

	if len(norm) != 1 {
		t.Fatalf("files = %d, want 1", len(norm))
	}
	if _, ok := norm["main.go"]["funcfoo(){"]; !ok {
		t.Error("missing normalized 'func foo() {'")
	}
	if _, ok := norm["main.go"]["returnx+y"]; !ok {
		t.Error("missing normalized 'return x + y'")
	}
}
