package service

import (
	"fmt"
	"strconv"
	"strings"
)

type attributionSummary struct {
	AILines        int
	TotalLines     int
	ExactLines     int
	FormattedLines int
	ModifiedLines  int
	Provider       string
}

func attributionSummaryFromResult(cr *commitAttrResult) (*attributionSummary, bool) {
	if cr == nil || cr.result == nil || cr.result.AILines == 0 {
		return nil, false
	}

	r := cr.result
	provider := ""
	if len(r.Providers) == 1 {
		provider = r.Providers[0].Provider
	} else if len(r.Providers) > 1 {
		provider = "multiple providers"
	}

	return &attributionSummary{
		AILines:        r.AILines,
		TotalLines:     r.TotalLines,
		ExactLines:     r.ExactLines,
		FormattedLines: r.FormattedLines,
		ModifiedLines:  r.ModifiedLines,
		Provider:       provider,
	}, true
}

func parseAttributionSummary(data []byte) (*attributionSummary, bool) {
	parts := strings.SplitN(strings.TrimSpace(string(data)), "|", 6)
	if len(parts) < 5 {
		return nil, false
	}

	aiLines, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, false
	}
	totalLines, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, false
	}
	exactLines, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, false
	}
	formattedLines, err := strconv.Atoi(parts[3])
	if err != nil {
		return nil, false
	}
	modifiedLines, err := strconv.Atoi(parts[4])
	if err != nil {
		return nil, false
	}

	provider := ""
	if len(parts) == 6 {
		provider = parts[5]
	}

	return &attributionSummary{
		AILines:        aiLines,
		TotalLines:     totalLines,
		ExactLines:     exactLines,
		FormattedLines: formattedLines,
		ModifiedLines:  modifiedLines,
		Provider:       provider,
	}, true
}

func (s attributionSummary) serialize() string {
	return fmt.Sprintf("%d|%d|%d|%d|%d|%s",
		s.AILines,
		s.TotalLines,
		s.ExactLines,
		s.FormattedLines,
		s.ModifiedLines,
		s.Provider,
	)
}

func (s attributionSummary) render() string {
	details := make([]string, 0, 4)
	if s.ExactLines > 0 {
		details = append(details, fmt.Sprintf("%d exact", s.ExactLines))
	}
	if s.FormattedLines > 0 {
		details = append(details, fmt.Sprintf("%d formatted", s.FormattedLines))
	}
	if s.ModifiedLines > 0 {
		details = append(details, fmt.Sprintf("%d modified", s.ModifiedLines))
	}
	if s.Provider != "" {
		details = append(details, s.Provider)
	}

	detailText := ""
	if len(details) > 0 {
		detailText = fmt.Sprintf(" (%s)", strings.Join(details, ", "))
	}

	return fmt.Sprintf(
		"Semantica: %d/%d AI-touched lines%s. Run `semantica blame HEAD` or `semantica explain HEAD` for more details.\n",
		s.AILines,
		s.TotalLines,
		detailText,
	)
}
