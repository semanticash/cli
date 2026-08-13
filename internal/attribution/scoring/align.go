package scoring

import "strings"

// ClaimLine is one ordered occurrence of tool-produced text.
type ClaimLine struct {
	Text string // trimmed content
	Norm string // whitespace-normalized content
}

// NewClaimLines preserves line order and leaves blank claims unmatched.
func NewClaimLines(lines []string) []ClaimLine {
	out := make([]ClaimLine, len(lines))
	for i, l := range lines {
		t := strings.TrimSpace(l)
		out[i] = ClaimLine{Text: t, Norm: NormalizeWhitespace(t)}
	}
	return out
}

// Alignment tiers mirror line-scoring tiers.
const (
	AlignNone       = 0
	AlignExact      = 1
	AlignNormalized = 2
)

// AlignedLine is the per-added-line result of an alignment.
type AlignedLine struct {
	ClaimIdx int // index into the claim slice, -1 when unmatched
	Tier     int // AlignExact, AlignNormalized, or AlignNone
}

// alignBudget includes the table's sentinel row and column.
const alignBudget = 1 << 22

// withinAlignBudget checks table dimensions without multiplication overflow.
func withinAlignBudget(nc, na int) bool {
	if nc <= 0 || na <= 0 {
		return true
	}
	return nc <= alignBudget/na
}

// AlignOrdered matches each claim to at most one added line while preserving
// order. Exact matches anchor the result; normalized matches fill only the
// gaps between anchors. Ties prefer the earliest added-line position.
//
// ok is false when the inputs exceed the alignment budget; the result
// is then nil and the caller must not attribute lines.
func AlignOrdered(claims []ClaimLine, added []string) (result []AlignedLine, ok bool) {
	if !withinAlignBudget(len(claims)+1, len(added)+1) {
		return nil, false
	}
	result = make([]AlignedLine, len(added))
	trimmed := make([]string, len(added))
	norm := make([]string, len(added))
	for j, l := range added {
		result[j] = AlignedLine{ClaimIdx: -1, Tier: AlignNone}
		trimmed[j] = strings.TrimSpace(l)
		norm[j] = NormalizeWhitespace(trimmed[j])
	}

	exactEq := func(i, j int) bool {
		return claims[i].Text != "" && claims[i].Text == trimmed[j]
	}
	alignSegment(claims, 0, len(claims), result, 0, len(added), exactEq, AlignExact)

	// Fill each gap without crossing exact anchors.
	normEq := func(i, j int) bool {
		return claims[i].Norm != "" && claims[i].Norm == norm[j]
	}
	segC, segA := 0, 0
	for j := 0; j <= len(added); j++ {
		if j < len(added) && result[j].Tier != AlignExact {
			continue
		}
		endC := len(claims)
		if j < len(added) {
			endC = result[j].ClaimIdx
		}
		alignSegment(claims, segC, endC, result, segA, j, normEq, AlignNormalized)
		if j < len(added) {
			segC = result[j].ClaimIdx + 1
			segA = j + 1
		}
	}
	return result, true
}

// alignSegment writes a deterministic LCS alignment into result.
func alignSegment(claims []ClaimLine, c0, c1 int, result []AlignedLine, a0, a1 int, eq func(i, j int) bool, tier int) {
	nc, na := c1-c0, a1-a0
	if nc <= 0 || na <= 0 || !withinAlignBudget(nc+1, na+1) {
		return
	}
	// A flat table keeps allocation proportional to the cell budget.
	w := na + 1
	dp := make([]int16, (nc+1)*w)
	at := func(i, j int) int { return i*w + j }
	usable := func(j int) bool { return result[a0+j].Tier == AlignNone }
	for i := nc - 1; i >= 0; i-- {
		for j := na - 1; j >= 0; j-- {
			best := dp[at(i+1, j)]
			if dp[at(i, j+1)] > best {
				best = dp[at(i, j+1)]
			}
			if usable(j) && eq(c0+i, a0+j) && dp[at(i+1, j+1)]+1 > best {
				best = dp[at(i+1, j+1)] + 1
			}
			dp[at(i, j)] = best
		}
	}
	i, j := 0, 0
	for i < nc && j < na {
		switch {
		case usable(j) && eq(c0+i, a0+j) && dp[at(i, j)] == dp[at(i+1, j+1)]+1:
			result[a0+j] = AlignedLine{ClaimIdx: c0 + i, Tier: tier}
			i++
			j++
		case dp[at(i, j+1)] > dp[at(i+1, j)]:
			j++
		default:
			// Preserve the earlier added position on ties.
			i++
		}
	}
}
