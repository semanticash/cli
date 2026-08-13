package toolsnap

import (
	"bytes"
	"context"
)

// Hunk is a contiguous, context-free change between two text blobs.
// Lines omit trailing newlines, and positions are 1-based.
type Hunk struct {
	OldStart int      `json:"old_start"`
	OldCount int      `json:"old_count"`
	NewStart int      `json:"new_start"`
	NewCount int      `json:"new_count"`
	OldLines []string `json:"old_lines"`
	NewLines []string `json:"new_lines"`
}

// maxDiffDepth bounds the Myers search per file. Exceeding it produces
// one deterministic hunk with the exact changed content.
const maxDiffDepth = 1024

// maxDiffWorkPerCapture bounds cumulative Myers work. Files consume the
// budget in canonical path order.
const maxDiffWorkPerCapture = 1 << 26

// maxDiffLinesPerFile bounds line-index memory. Larger files produce
// truncated, file-level evidence without materializing line slices.
const maxDiffLinesPerFile = 1 << 20

// lineCount counts newline-delimited lines without materializing
// them.
func lineCount(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	n := bytes.Count(content, []byte("\n"))
	if content[len(content)-1] != '\n' {
		n++
	}
	return n
}

// diffLines diffs one pair without a cross-file budget or deadline;
// capture paths use diffLinesBudget.
func diffLines(oldContent, newContent []byte) []Hunk {
	hunks, _ := diffLinesBudget(context.Background(), oldContent, newContent, nil)
	return hunks
}

// diffLinesBudget compares byte-exact lines without normalization or
// context. It bounds search depth and checks cancellation during work.
func diffLinesBudget(ctx context.Context, oldContent, newContent []byte, budget *int64) ([]Hunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Charge before materializing line slices.
	if budget != nil {
		linear := int64(lineCount(oldContent) + lineCount(newContent))
		if linear == 0 {
			linear = 1
		}
		depth := int64(maxDiffDepth)
		if allowed := *budget / linear; allowed < depth {
			depth = allowed
		}
		*budget -= linear * depth
		if depth <= 0 {
			hunks := coarseHunkFromContent(oldContent, newContent)
			// Do not return evidence completed after cancellation.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return hunks, nil
		}
		return diffSplit(ctx, oldContent, newContent, int(depth))
	}
	return diffSplit(ctx, oldContent, newContent, maxDiffDepth)
}

func coarseHunkFromContent(oldContent, newContent []byte) []Hunk {
	a := splitLines(oldContent)
	b := splitLines(newContent)
	prefix, suffix := trimCommon(a, b)
	ma := a[prefix : len(a)-suffix]
	mb := b[prefix : len(b)-suffix]
	if len(ma) == 0 && len(mb) == 0 {
		return nil
	}
	return []Hunk{{
		OldStart: prefix + 1, OldCount: len(ma),
		NewStart: prefix + 1, NewCount: len(mb),
		OldLines: append([]string(nil), ma...),
		NewLines: append([]string(nil), mb...),
	}}
}

func trimCommon(a, b []string) (prefix, suffix int) {
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	return prefix, suffix
}

func diffSplit(ctx context.Context, oldContent, newContent []byte, depth int) ([]Hunk, error) {
	a := splitLines(oldContent)
	b := splitLines(newContent)

	// Restrict the search to the changed middle region.
	prefix, suffix := trimCommon(a, b)
	ma := a[prefix : len(a)-suffix]
	mb := b[prefix : len(b)-suffix]
	// Check cancellation after preprocessing, including no-op diffs.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ma) == 0 && len(mb) == 0 {
		return nil, nil
	}

	ops, err := myersOps(ctx, ma, mb, depth)
	if err != nil {
		return nil, err
	}
	if ops == nil {
		// Depth cap exceeded: one coarse hunk over the middle region.
		return []Hunk{{
			OldStart: prefix + 1, OldCount: len(ma),
			NewStart: prefix + 1, NewCount: len(mb),
			OldLines: append([]string(nil), ma...),
			NewLines: append([]string(nil), mb...),
		}}, nil
	}
	return opsToHunks(ops, ma, mb, prefix), nil
}

type opKind byte

const (
	opEq opKind = iota
	opDel
	opIns
)

// myersOps runs a bounded Myers line diff. It returns nil operations
// when the edit distance exceeds depthCap.
func myersOps(ctx context.Context, a, b []string, depthCap int) ([]opKind, error) {
	n, m := len(a), len(b)
	maxD := n + m
	if maxD > depthCap {
		maxD = depthCap
	}
	offset := maxD
	v := make([]int, 2*maxD+2)
	trace := make([][]int, 0, maxD+1)

	var found bool
	for d := 0; d <= maxD; d++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		vc := make([]int, len(v))
		copy(vc, v)
		trace = append(trace, vc)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				found = true
			}
		}
		if found {
			break
		}
	}
	if !found {
		return nil, nil
	}

	// Backtrack to the edit sequence.
	var ops []opKind
	x, y := n, m
	for d := len(trace) - 1; d > 0; d-- {
		vd := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && vd[offset+k-1] < vd[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := vd[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			ops = append(ops, opEq)
			x--
			y--
		}
		if x == prevX {
			ops = append(ops, opIns)
			y--
		} else {
			ops = append(ops, opDel)
			x--
		}
	}
	for x > 0 && y > 0 {
		ops = append(ops, opEq)
		x--
		y--
	}
	for x > 0 {
		ops = append(ops, opDel)
		x--
	}
	for y > 0 {
		ops = append(ops, opIns)
		y--
	}
	// Reverse to forward order.
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops, nil
}

// opsToHunks groups consecutive changes into 1-based hunks.
func opsToHunks(ops []opKind, a, b []string, prefix int) []Hunk {
	var hunks []Hunk
	ai, bi := 0, 0
	i := 0
	for i < len(ops) {
		if ops[i] == opEq {
			ai++
			bi++
			i++
			continue
		}
		h := Hunk{OldStart: prefix + ai + 1, NewStart: prefix + bi + 1}
		for i < len(ops) && ops[i] != opEq {
			switch ops[i] {
			case opDel:
				h.OldLines = append(h.OldLines, a[ai])
				ai++
			case opIns:
				h.NewLines = append(h.NewLines, b[bi])
				bi++
			}
			i++
		}
		h.OldCount = len(h.OldLines)
		h.NewCount = len(h.NewLines)
		hunks = append(hunks, h)
	}
	return hunks
}

// splitLines splits content into lines without trailing newlines. A
// missing final newline is not representable in the line slice;
// callers record it as file-level metadata.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	trimmed := bytes.TrimSuffix(content, []byte("\n"))
	parts := bytes.Split(trimmed, []byte("\n"))
	lines := make([]string, len(parts))
	for i, p := range parts {
		lines[i] = string(p)
	}
	return lines
}

// isBinary mirrors git's heuristic: a NUL byte in the leading window
// marks content as binary. Binary files contribute file-touch
// evidence only.
func isBinary(content []byte) bool {
	window := content
	if len(window) > 8000 {
		window = window[:8000]
	}
	return bytes.IndexByte(window, 0) >= 0
}
