package toolsnap

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDiffLines(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		want     []Hunk
	}{
		{
			name: "identical",
			old:  "a\nb\nc\n",
			new:  "a\nb\nc\n",
			want: nil,
		},
		{
			name: "single line edit",
			old:  "a\nb\nc\n",
			new:  "a\nB\nc\n",
			want: []Hunk{{OldStart: 2, OldCount: 1, NewStart: 2, NewCount: 1, OldLines: []string{"b"}, NewLines: []string{"B"}}},
		},
		{
			name: "insertion",
			old:  "a\nc\n",
			new:  "a\nb\nc\n",
			want: []Hunk{{OldStart: 2, OldCount: 0, NewStart: 2, NewCount: 1, NewLines: []string{"b"}}},
		},
		{
			name: "deletion",
			old:  "a\nb\nc\n",
			new:  "a\nc\n",
			want: []Hunk{{OldStart: 2, OldCount: 1, NewStart: 2, NewCount: 0, OldLines: []string{"b"}}},
		},
		{
			name: "two separated hunks",
			old:  "a\nb\nc\nd\ne\n",
			new:  "A\nb\nc\nd\nE\n",
			want: []Hunk{
				{OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1, OldLines: []string{"a"}, NewLines: []string{"A"}},
				{OldStart: 5, OldCount: 1, NewStart: 5, NewCount: 1, OldLines: []string{"e"}, NewLines: []string{"E"}},
			},
		},
		{
			name: "create from empty",
			old:  "",
			new:  "x\ny\n",
			want: []Hunk{{OldStart: 1, OldCount: 0, NewStart: 1, NewCount: 2, NewLines: []string{"x", "y"}}},
		},
		{
			name: "delete to empty",
			old:  "x\ny\n",
			new:  "",
			want: []Hunk{{OldStart: 1, OldCount: 2, NewStart: 1, NewCount: 0, OldLines: []string{"x", "y"}}},
		},
		{
			name: "whitespace is byte-exact",
			old:  "a \n",
			new:  "a\n",
			want: []Hunk{{OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 1, OldLines: []string{"a "}, NewLines: []string{"a"}}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diffLines([]byte(c.old), []byte(c.new))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("diff = %+v\nwant  %+v", got, c.want)
			}
		})
	}
}

// TestDiffLinesRoundTrip verifies exact reconstruction from hunks.
func TestDiffLinesRoundTrip(t *testing.T) {
	for seed := 0; seed < 20; seed++ {
		oldLines := make([]string, 0, 40)
		for i := 0; i < 40; i++ {
			oldLines = append(oldLines, fmt.Sprintf("line-%d-%d", seed, i))
		}
		newLines := append([]string(nil), oldLines...)
		// Apply deterministic edits for this seed.
		newLines[(seed*7)%40] = "changed"
		if seed%3 == 0 {
			newLines = append(newLines[:(seed*5)%30], newLines[(seed*5)%30+2:]...)
		}
		if seed%4 == 1 {
			at := (seed * 11) % len(newLines)
			newLines = append(newLines[:at], append([]string{"inserted-a", "inserted-b"}, newLines[at:]...)...)
		}
		oldContent := strings.Join(oldLines, "\n") + "\n"
		newContent := strings.Join(newLines, "\n") + "\n"

		hunks := diffLines([]byte(oldContent), []byte(newContent))
		rebuilt := applyHunks(oldLines, hunks)
		if !reflect.DeepEqual(rebuilt, newLines) {
			t.Fatalf("seed %d: hunks do not reconstruct new content\nhunks: %+v", seed, hunks)
		}
	}
}

// applyHunks replays hunks over the old lines.
func applyHunks(oldLines []string, hunks []Hunk) []string {
	out := make([]string, 0, len(oldLines))
	pos := 0 // 0-based index into oldLines
	for _, h := range hunks {
		for pos < h.OldStart-1 {
			out = append(out, oldLines[pos])
			pos++
		}
		out = append(out, h.NewLines...)
		pos += h.OldCount
	}
	out = append(out, oldLines[pos:]...)
	return out
}

func TestDiffLinesDepthCapFallsBackToCoarseHunk(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&oldB, "old-%d\n", i)
		fmt.Fprintf(&newB, "new-%d\n", i)
	}
	hunks := diffLines([]byte(oldB.String()), []byte(newB.String()))
	if len(hunks) != 1 {
		t.Fatalf("hunks = %d, want one coarse hunk", len(hunks))
	}
	h := hunks[0]
	if h.OldCount != 2000 || h.NewCount != 2000 || h.OldStart != 1 || h.NewStart != 1 {
		t.Errorf("coarse hunk = %d@%d -> %d@%d", h.OldCount, h.OldStart, h.NewCount, h.NewStart)
	}
	if h.OldLines[0] != "old-0" || h.NewLines[1999] != "new-1999" {
		t.Errorf("coarse hunk content mismatch")
	}
}

func TestDiffLinesDeterministic(t *testing.T) {
	old := []byte("a\nb\nc\nd\n")
	new := []byte("a\nx\nc\ny\nd\n")
	first := diffLines(old, new)
	for i := 0; i < 5; i++ {
		if got := diffLines(old, new); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
}

func TestIsBinary(t *testing.T) {
	if isBinary([]byte("plain text\nwith lines\n")) {
		t.Error("text classified binary")
	}
	if !isBinary([]byte{0x89, 'P', 'N', 'G', 0x00, 0x01}) {
		t.Error("NUL content not classified binary")
	}
	tail := append(make([]byte, 9000), 0x00)
	for i := range tail[:9000] {
		tail[i] = 'a'
	}
	// NUL beyond the leading window: treated as text, matching git.
	if isBinary(tail) {
		t.Error("NUL beyond window classified binary")
	}
}
