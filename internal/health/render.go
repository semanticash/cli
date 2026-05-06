package health

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// categoryOrder is the display order in text output. Entries not in
// the map fall to the end alphabetically.
var categoryOrder = map[string]int{
	"binary":   0,
	"launcher": 1,
	"hooks":    2,
	"state":    3,
}

// categoryTitle is the human-readable label for each category.
var categoryTitle = map[string]string{
	"binary":   "Binary",
	"launcher": "Launcher",
	"hooks":    "Hooks",
	"state":    "Capture state",
}

// RenderText writes a human-readable diagnostic to w.
func RenderText(w io.Writer, r Report) error {
	groups := groupByCategory(r.Checks)
	cats := orderedCategories(groups)

	var b strings.Builder
	b.WriteString("semantica doctor\n")

	for _, cat := range cats {
		title, ok := categoryTitle[cat]
		if !ok {
			title = cat
		}
		b.WriteString("\n  ")
		b.WriteString(title)
		b.WriteString("\n")
		for _, c := range groups[cat] {
			b.WriteString("    [")
			b.WriteString(string(c.Status))
			b.WriteString("] ")
			b.WriteString(c.Message)
			b.WriteString("\n")
			if c.Remediation != "" {
				b.WriteString("           remediation: ")
				b.WriteString(c.Remediation)
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\n  Result: ")
	b.WriteString(string(r.Result))
	if r.Summary.Fail+r.Summary.Warn > 0 {
		b.WriteString(" (")
		if r.Summary.Fail > 0 {
			b.WriteString(itoa(r.Summary.Fail))
			b.WriteString(" issue")
			if r.Summary.Fail != 1 {
				b.WriteString("s")
			}
		}
		if r.Summary.Fail > 0 && r.Summary.Warn > 0 {
			b.WriteString(", ")
		}
		if r.Summary.Warn > 0 {
			b.WriteString(itoa(r.Summary.Warn))
			b.WriteString(" warning")
			if r.Summary.Warn != 1 {
				b.WriteString("s")
			}
		}
		b.WriteString(")")
	}
	b.WriteString("\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// RenderJSON writes the report as pretty-printed JSON.
func RenderJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func groupByCategory(checks []Check) map[string][]Check {
	groups := map[string][]Check{}
	for _, c := range checks {
		groups[c.Category] = append(groups[c.Category], c)
	}
	return groups
}

func orderedCategories(groups map[string][]Check) []string {
	cats := make([]string, 0, len(groups))
	for c := range groups {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool {
		oi, oki := categoryOrder[cats[i]]
		oj, okj := categoryOrder[cats[j]]
		if !oki {
			oi = 100
		}
		if !okj {
			oj = 100
		}
		if oi != oj {
			return oi < oj
		}
		return cats[i] < cats[j]
	})
	return cats
}
