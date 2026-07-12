package carryforward

import "testing"

func TestIdentifyCreatedCandidates(t *testing.T) {
	tests := []struct {
		name         string
		filesCreated []string
		manifest     []ManifestEntry
		want         map[string]bool
	}{
		{
			name:         "created file in manifest",
			filesCreated: []string{"a.go"},
			manifest:     []ManifestEntry{{Path: "a.go"}, {Path: "b.go"}},
			want:         map[string]bool{"a.go": true},
		},
		{
			name:         "created file NOT in manifest",
			filesCreated: []string{"new.go"},
			manifest:     []ManifestEntry{{Path: "b.go"}},
			want:         nil,
		},
		{
			name:         "no created files",
			filesCreated: nil,
			manifest:     []ManifestEntry{{Path: "a.go"}},
			want:         nil,
		},
		{
			name:         "mix of in and not in manifest",
			filesCreated: []string{"a.go", "new.go"},
			manifest:     []ManifestEntry{{Path: "a.go"}, {Path: "b.go"}},
			want:         map[string]bool{"a.go": true},
		},
		{
			name:         "empty manifest",
			filesCreated: []string{"a.go"},
			manifest:     nil,
			want:         nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IdentifyCreatedCandidates(tt.filesCreated, tt.manifest)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("missing key %q", k)
				}
			}
		})
	}
}

// Modified-file eligibility relies on scoring to reject stale evidence.
func TestIdentifyModifiedCandidates(t *testing.T) {
	tests := []struct {
		name        string
		filesEdited []string
		want        map[string]bool
	}{
		{
			name:        "single edited file",
			filesEdited: []string{"a.go"},
			want:        map[string]bool{"a.go": true},
		},
		{
			name:        "multiple edited files",
			filesEdited: []string{"a.go", "b.go", "c.go"},
			want:        map[string]bool{"a.go": true, "b.go": true, "c.go": true},
		},
		{
			name:        "empty input returns nil",
			filesEdited: nil,
			want:        nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IdentifyModifiedCandidates(tt.filesEdited)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("missing key %q", k)
				}
			}
		})
	}
}
