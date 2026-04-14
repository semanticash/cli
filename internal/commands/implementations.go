package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/service/implementations"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

const implementationPickerTitleWidth = 54
const implementationStoryWrapWidth = 86

var errNoImplementations = fmt.Errorf("no implementations found")

type implementationCardFieldJSON struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type implementationCardJSON struct {
	Title         string                        `json:"title"`
	Subtitle      string                        `json:"subtitle"`
	Context       string                        `json:"context,omitempty"`
	AIAttribution string                        `json:"ai_attribution,omitempty"`
	Story         []string                      `json:"story,omitempty"`
	Repos         []string                      `json:"repos,omitempty"`
	Commits       []string                      `json:"commits,omitempty"`
	Stats         []implementationCardFieldJSON `json:"stats,omitempty"`
	Details       []string                      `json:"details,omitempty"`
	Timeline      []string                      `json:"timeline,omitempty"`
}

type implementationDetailJSON struct {
	*implementations.ImplementationDetail
	AIAttribution string                 `json:"ai_attribution,omitempty"`
	Card          implementationCardJSON `json:"card"`
}

func NewImplementationsCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		asJSON        bool
		all           bool
		includeSingle bool
		limit         int64
		verbose       bool
	)

	cmd := &cobra.Command{
		Use:     "implementations [implementation_id]",
		Aliases: []string{"impl"},
		Short:   "List or inspect cross-repo implementations",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			listInput := implementations.ListInput{
				Limit:         limit,
				All:           all,
				IncludeSingle: includeSingle,
			}

			if len(args) == 1 {
				return showImplementation(cmd, out, args[0], asJSON, verbose)
			}

			if !asJSON && isTerminal() && isTerminalWriter(out) {
				implID, err := pickImplementation(cmd.Context(), listInput)
				if err != nil {
					if err == errNoImplementations {
						_, _ = fmt.Fprintln(out, "No implementations found.")
						return nil
					}
					return err
				}
				return showImplementation(cmd, out, implID, false, verbose)
			}

			return listImplementations(cmd, out, listInput, asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&all, "all", false, "Show all implementations including old dormant and single-repo")
	cmd.Flags().BoolVar(&includeSingle, "include-single", false, "Include single-repo implementations")
	cmd.Flags().Int64Var(&limit, "limit", 20, "Max implementations to list")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show full raw timeline details")

	// Subcommands
	cmd.AddCommand(newImplCloseCmd())
	cmd.AddCommand(newImplLinkCmd(rootOpts))
	cmd.AddCommand(newImplMergeCmd())

	return cmd
}

// --- close ---

func newImplCloseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "close <implementation_id>",
		Short: "Close an implementation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := implementations.Close(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Implementation %s closed.\n", args[0])
			return nil
		},
	}
}

// --- link ---

func newImplLinkCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		sessionID string
		commitSHA string
		repoPath  string
		force     bool
	)

	cmd := &cobra.Command{
		Use:   "link <implementation_id>",
		Short: "Link a session or commit to an implementation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			implID := args[0]

			if sessionID == "" && commitSHA == "" {
				return fmt.Errorf("specify --session or --commit")
			}
			if sessionID != "" && commitSHA != "" {
				return fmt.Errorf("specify only one of --session or --commit")
			}

			repo := repoPath
			if repo == "" {
				repo = rootOpts.RepoPath
			}

			if sessionID != "" {
				result, err := implementations.LinkSession(cmd.Context(), implementations.LinkSessionInput{
					ImplementationID: implID,
					SessionID:        sessionID,
					RepoPath:         repo,
					Force:            force,
				})
				if err != nil {
					return err
				}
				if result.MovedFrom != "" {
					_, _ = fmt.Fprintf(out, "Session %s moved from %s to %s.\n",
						result.LinkedSessionID, util.ShortID(result.MovedFrom), implID)
				} else {
					_, _ = fmt.Fprintf(out, "Session %s linked to %s.\n",
						result.LinkedSessionID, implID)
				}
				return nil
			}

			if err := implementations.LinkCommit(cmd.Context(), implementations.LinkCommitInput{
				ImplementationID: implID,
				CommitHash:       commitSHA,
				RepoPath:         repo,
			}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "Commit %s linked to %s.\n", commitSHA, implID)
			return nil
		},
	}

	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID to link")
	cmd.Flags().StringVar(&commitSHA, "commit", "", "Commit SHA to link")
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository path (default: current)")
	cmd.Flags().BoolVar(&force, "force", false, "Move session from another implementation without confirmation")

	return cmd
}

// --- merge ---

func newImplMergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <target_id> <source_id>",
		Short: "Merge source implementation into target",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := implementations.Merge(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Merged %s into %s.\n",
				util.ShortID(result.SourceID), util.ShortID(result.TargetID))
			return nil
		},
	}
}

// --- output helpers ---

func listImplementations(cmd *cobra.Command, out io.Writer, in implementations.ListInput, asJSON bool) error {
	result, err := implementations.List(cmd.Context(), in)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	if len(result.Items) == 0 {
		_, _ = fmt.Fprintln(out, "No implementations found.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "IMPLEMENTATIONS\n\n")
	_, _ = fmt.Fprintf(out, "%-10s %-28s %-16s %-10s %-8s %s\n",
		"ID", "Title", "Repos", "State", "Last", "Commits")

	for _, item := range result.Items {
		id := util.ShortID(item.ImplementationID)
		title := displayImplementationTitle(item.Title, 26)
		repos := displayImplementationRepos(item.Repos, 14)
		_, _ = fmt.Fprintf(out, "%-10s %-28s %-16s %-10s %-8s %d\n",
			id, title, repos, item.State, service.RelativeTime(item.LastActivityAt), item.CommitCount)
	}

	return nil
}

func showImplementation(cmd *cobra.Command, out io.Writer, implID string, asJSON, verbose bool) error {
	detail, err := implementations.GetDetail(cmd.Context(), implID)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(buildImplementationJSON(detail, verbose))
	}

	if isTerminalWriter(out) {
		_, _ = lipgloss.Fprintln(out, renderImplementationCard(detail, verbose))
		return nil
	}

	_, _ = fmt.Fprint(out, renderImplementationPlain(detail, verbose))

	return nil
}

func pickImplementation(ctx context.Context, in implementations.ListInput) (string, error) {
	result, err := implementations.List(ctx, in)
	if err != nil {
		return "", err
	}
	if len(result.Items) == 0 {
		return "", errNoImplementations
	}

	options := make([]huh.Option[string], len(result.Items))
	for i, item := range result.Items {
		options[i] = huh.NewOption(formatImplementationOption(item), item.ImplementationID)
	}

	var selected string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(implementationPickerTitle()).
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		return "", fmt.Errorf("no implementation selected")
	}
	if selected == "" {
		return "", fmt.Errorf("no implementation selected")
	}
	return selected, nil
}

func formatImplementationOption(item implementations.ListItem) string {
	title := displayImplementationTitle(item.Title, implementationPickerTitleWidth)
	repos := displayImplementationRepos(item.Repos, 0)
	commitLabel := "commits"
	if item.CommitCount == 1 {
		commitLabel = "commit"
	}
	commitText := fmt.Sprintf("%d %s", item.CommitCount, commitLabel)
	lastActivity := service.RelativeTime(item.LastActivityAt)
	labelStyle := lipgloss.NewStyle().Faint(true)
	repoStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
	return fmt.Sprintf("%-8s  %-54s  %-8s  %-5s  %s\n%s %s",
		util.ShortID(item.ImplementationID),
		title,
		item.State,
		lastActivity,
		commitText,
		labelStyle.Render("Repositories:"),
		repoStyle.Render(repos))
}

func implementationPickerTitle() string {
	return fmt.Sprintf(
		"Select an implementation\n  %-8s  %-54s  %-8s  %-5s  %s",
		"ID",
		"TITLE",
		"STATE",
		"LAST",
		"COMMITS",
	)
}

func displayImplementationTitle(title string, maxLen int) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "-"
	}
	return truncateDisplay(title, maxLen)
}

func displayImplementationRepos(repos []implementations.RepoSummary, maxLen int) string {
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.DisplayName)
	}
	return truncateDisplay(strings.Join(names, ", "), maxLen)
}

func truncateDisplay(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func renderImplementationPlain(detail *implementations.ImplementationDetail, verbose bool) string {
	var b strings.Builder

	b.WriteString(implementationDisplayTitle(detail) + "\n")
	b.WriteString("Implementation: " + util.ShortID(detail.ImplementationID) + "\n")
	b.WriteString("State: " + detail.State + "\n")
	b.WriteString("Last activity: " + service.RelativeTime(detail.LastActivityAt) + "\n")
	if ctx := implementationContextLine(detail); ctx != "" {
		b.WriteString(ctx + "\n")
	}
	if story := buildSummaryLines(detail); len(story) > 0 {
		b.WriteString("\nStory\n")
		for _, line := range story {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\nRepos\n")
	for _, line := range buildRepoLines(detail) {
		b.WriteString("  " + line + "\n")
	}

	if commits := buildCommitLines(detail); len(commits) > 0 {
		b.WriteString("\nCommits\n")
		for _, line := range commits {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\nStats\n")
	for _, field := range implementationStats(detail) {
		b.WriteString("  " + field.Label + ": " + field.Value + "\n")
	}

	if verbose {
		if details := buildDetailLines(detail); len(details) > 0 {
			b.WriteString("\nDetails\n")
			for _, line := range details {
				b.WriteString("  " + line + "\n")
			}
		}
		timeline := buildVerboseTimelineLines(detail)
		if len(timeline) > 0 {
			b.WriteString("\nTimeline\n")
			for _, line := range timeline {
				b.WriteString("  " + line + "\n")
			}
		}
	}

	return b.String()
}

func renderImplementationCard(detail *implementations.ImplementationDetail, verbose bool) string {
	theme := enableCardTheme()

	boxStyle := theme.Focused.Card.
		UnsetBorderLeft().
		BorderStyle(lipgloss.NormalBorder()).
		BorderTop(true).
		BorderRight(true).
		BorderBottom(true).
		BorderLeft(true).
		Padding(0, 1)
	titleStyle := theme.Focused.SelectedOption.Bold(true)
	subtleStyle := theme.Focused.Description
	labelStyle := lipgloss.NewStyle().Bold(true)
	valueStyle := lipgloss.NewStyle()
	sectionBodyStyle := lipgloss.NewStyle().PaddingLeft(2)

	header := titleStyle.Render(implementationDisplayTitle(detail))
	subtitle := subtleStyle.Render(fmt.Sprintf("Implementation %s | %s | last activity %s",
		util.ShortID(detail.ImplementationID),
		detail.State,
		service.RelativeTime(detail.LastActivityAt)))

	sections := []string{header, subtitle}
	if ctx := implementationContextLine(detail); ctx != "" {
		sections = append(sections, subtleStyle.Render(ctx))
	}
	if story := buildSummaryLines(detail); len(story) > 0 {
		sections = append(sections, "", renderImplementationSectionCard("Story", story, labelStyle, valueStyle, sectionBodyStyle))
	}

	if repos := buildRepoLines(detail); len(repos) > 0 {
		sections = append(sections, "", renderImplementationSectionCard("Repos", repos, labelStyle, valueStyle, sectionBodyStyle))
	}

	if commits := buildCommitLines(detail); len(commits) > 0 {
		sections = append(sections, "", renderImplementationSectionCard("Commits", commits, labelStyle, valueStyle, sectionBodyStyle))
	}

	stats := make([]string, 0, len(implementationStats(detail)))
	for _, field := range implementationStats(detail) {
		stats = append(stats, enableCardRow(labelStyle, valueStyle, field.Label, field.Value))
	}
	if len(stats) > 0 {
		sections = append(sections, "", renderImplementationSectionCard("Stats", stats, labelStyle, valueStyle, sectionBodyStyle))
	}

	if verbose {
		if details := buildDetailLines(detail); len(details) > 0 {
			sections = append(sections, "", renderImplementationSectionCard("Details", details, labelStyle, valueStyle, sectionBodyStyle))
		}
		if timeline := buildVerboseTimelineLines(detail); len(timeline) > 0 {
			sections = append(sections, "", renderImplementationSectionCard("Timeline", timeline, labelStyle, valueStyle, sectionBodyStyle))
		}
	}

	return boxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, sections...))
}

func renderImplementationSectionCard(
	title string,
	lines []string,
	labelStyle, valueStyle, sectionBodyStyle lipgloss.Style,
) string {
	sectionTitle := labelStyle.Render(title)
	body := make([]string, 0, len(lines))
	for _, line := range lines {
		body = append(body, sectionBodyStyle.Render(valueStyle.Render(line)))
	}
	return lipgloss.JoinVertical(lipgloss.Left, append([]string{sectionTitle}, body...)...)
}

func implementationDisplayTitle(detail *implementations.ImplementationDetail) string {
	title := strings.TrimSpace(detail.Title)
	if title == "" {
		return "Untitled implementation"
	}
	return title
}

func buildRepoLines(detail *implementations.ImplementationDetail) []string {
	lines := make([]string, 0, len(detail.Repos))
	for _, r := range detail.Repos {
		lines = append(lines, fmt.Sprintf("%-14s %-11s first seen %s, %d sessions",
			r.DisplayName,
			r.Role,
			service.RelativeTime(r.FirstSeenAt),
			r.SessionCount))
	}
	return lines
}

func buildCommitLines(detail *implementations.ImplementationDetail) []string {
	lines := make([]string, 0, len(detail.Commits))
	for _, c := range detail.Commits {
		subject := strings.TrimSpace(c.Subject)
		if subject == "" {
			subject = "(no subject)"
		}
		lines = append(lines, fmt.Sprintf("%-12s %-8s %s",
			c.DisplayName,
			util.ShortID(c.CommitHash),
			subject))
	}
	return lines
}

func implementationStats(detail *implementations.ImplementationDetail) []statusField {
	repoNames := make([]string, 0, len(detail.Repos))
	for _, r := range detail.Repos {
		repoNames = append(repoNames, r.DisplayName)
	}

	fields := []statusField{
		{Label: "Repos", Value: strings.Join(repoNames, ", ")},
		{Label: "Implementation sessions", Value: fmt.Sprintf("%d", len(detail.Sessions))},
		{Label: "Session details", Value: implementationSessionDetails(detail)},
		{Label: "Commits", Value: fmt.Sprintf("%d", len(detail.Commits))},
	}
	if ai := implementationAIAttribution(detail); ai != "" {
		fields = append(fields, statusField{
			Label: "AI attribution",
			Value: ai,
		})
	}
	if detail.TotalTokensIn > 0 || detail.TotalTokensOut > 0 || detail.TotalTokensCached > 0 {
		tokenValue := fmt.Sprintf("%s in / %s out",
			service.CompactTokens(detail.TotalTokensIn),
			service.CompactTokens(detail.TotalTokensOut))
		if detail.TotalTokensCached > 0 {
			tokenValue += fmt.Sprintf(" (+%s cached)", service.CompactTokens(detail.TotalTokensCached))
		}
		fields = append(fields, statusField{
			Label: "Tokens",
			Value: tokenValue,
		})
	}
	return fields
}

func implementationAIAttribution(detail *implementations.ImplementationDetail) string {
	if len(detail.RepoAttribution) == 0 {
		return ""
	}
	parts := make([]string, 0, len(detail.RepoAttribution))
	for _, repo := range detail.RepoAttribution {
		parts = append(parts, fmt.Sprintf("%.0f%% %s", repo.AIPercentage, repo.DisplayName))
	}
	return strings.Join(parts, " · ")
}

func buildImplementationJSON(detail *implementations.ImplementationDetail, verbose bool) implementationDetailJSON {
	aiAttribution := implementationAIAttribution(detail)
	card := implementationCardJSON{
		Title: implementationDisplayTitle(detail),
		Subtitle: fmt.Sprintf("Implementation %s | %s | last activity %s",
			util.ShortID(detail.ImplementationID),
			detail.State,
			service.RelativeTime(detail.LastActivityAt)),
		Context:       implementationContextLine(detail),
		AIAttribution: aiAttribution,
		Story:         buildSummaryLines(detail),
		Repos:         buildRepoLines(detail),
		Commits:       buildCommitLines(detail),
	}

	stats := implementationStats(detail)
	card.Stats = make([]implementationCardFieldJSON, 0, len(stats))
	for _, field := range stats {
		card.Stats = append(card.Stats, implementationCardFieldJSON(field))
	}

	if verbose {
		card.Details = buildDetailLines(detail)
		card.Timeline = buildVerboseTimelineLines(detail)
	}

	return implementationDetailJSON{
		ImplementationDetail: detail,
		AIAttribution:        aiAttribution,
		Card:                 card,
	}
}

func buildSummaryLines(detail *implementations.ImplementationDetail) []string {
	summary := strings.TrimSpace(detail.Summary)
	if summary == "" {
		return nil
	}
	return wrapImplementationText(summary, implementationStoryWrapWidth)
}

func implementationSessionDetails(detail *implementations.ImplementationDetail) string {
	parts := make([]string, 0, len(detail.Repos))
	for _, repo := range detail.Repos {
		parts = append(parts, fmt.Sprintf("%d in %s", repo.SessionCount, repo.DisplayName))
	}
	return strings.Join(parts, ", ")
}

func extractSummaryPath(summary, repoName string) string {
	start := strings.LastIndex(summary, "(")
	end := strings.LastIndex(summary, ")")
	if start == -1 || end == -1 || end <= start+1 {
		return ""
	}
	path := strings.TrimSpace(summary[start+1 : end])
	if !platform.LooksAbsolutePath(path) {
		return ""
	}
	if repoName != "" {
		needle := "/" + repoName + "/"
		if idx := strings.Index(path, needle); idx != -1 {
			return path[idx+len(needle):]
		}
	}
	return filepath.Base(path)
}

func isInternalStoryPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasPrefix(lower, ".claude/") ||
		strings.HasPrefix(lower, ".cursor/") ||
		strings.HasPrefix(lower, ".gemini/") ||
		strings.HasPrefix(lower, ".semantica/") ||
		strings.HasPrefix(lower, ".git/") ||
		strings.HasPrefix(lower, ".kiro/") ||
		lower == ".gitignore"
}

func implementationContextLine(detail *implementations.ImplementationDetail) string {
	startRepo, provider := implementationStart(detail)
	if startRepo == "" {
		return ""
	}
	if provider != "" {
		return fmt.Sprintf("Started in %s repo (by %s)", startRepo, provider)
	}
	return fmt.Sprintf("Started in %s", startRepo)
}

func implementationStart(detail *implementations.ImplementationDetail) (string, string) {
	provider := ""
	if len(detail.Sessions) > 0 {
		provider = implementationProviderDisplayName(detail.Sessions[0].Provider)
	}
	for _, repo := range detail.Repos {
		if repo.Role == "origin" {
			return repo.DisplayName, provider
		}
	}
	if len(detail.Repos) > 0 {
		return detail.Repos[0].DisplayName, provider
	}
	if len(detail.Sessions) > 0 {
		first := detail.Sessions[0]
		repo := filepath.Base(strings.TrimSpace(first.SourceProjectPath))
		if repo == "." || repo == string(filepath.Separator) {
			repo = ""
		}
		return repo, provider
	}
	return "", ""
}

func implementationProviderDisplayName(provider string) string {
	switch provider {
	case "claude_code":
		return "Claude"
	case "cursor":
		return "Cursor"
	case "gemini_cli":
		return "Gemini"
	case "copilot":
		return "Copilot"
	default:
		return ""
	}
}

func wrapImplementationText(text string, width int) []string {
	if width <= 0 {
		width = implementationStoryWrapWidth
	}

	paragraphs := strings.Split(strings.TrimSpace(text), "\n")
	lines := make([]string, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			continue
		}
		current := words[0]
		for _, word := range words[1:] {
			if len(current)+1+len(word) > width {
				lines = append(lines, current)
				current = word
				continue
			}
			current += " " + word
		}
		lines = append(lines, current)
	}
	return lines
}

func buildDetailLines(detail *implementations.ImplementationDetail) []string {
	type item struct {
		repo string
		path string
		op   string
	}
	seen := map[string]bool{}
	items := make([]item, 0, 8)
	for i := len(detail.Timeline) - 1; i >= 0; i-- {
		entry := detail.Timeline[i]
		if entry.Kind == "commit" {
			continue
		}
		path := entry.FilePath
		op := entry.FileOp
		if path == "" {
			path = extractSummaryPath(entry.Summary, entry.RepoName)
		}
		if path == "" || isInternalStoryPath(path) {
			continue
		}
		key := entry.RepoName + "|" + path + "|" + op
		if seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, item{repo: entry.RepoName, path: path, op: op})
		if len(items) == 8 {
			break
		}
	}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		suffix := ""
		if it.op != "" {
			suffix = " (" + it.op + ")"
		}
		lines = append(lines, fmt.Sprintf("%-12s %s%s", it.repo, it.path, suffix))
	}
	return lines
}

func buildVerboseTimelineLines(detail *implementations.ImplementationDetail) []string {
	lines := make([]string, 0, len(detail.Timeline))
	for _, e := range detail.Timeline {
		repo := e.RepoName
		if e.CrossRepo {
			repo = "-> " + repo
		}
		summary := strings.TrimSpace(e.Summary)
		if e.Kind == "commit" && strings.HasPrefix(strings.ToLower(summary), "commit ") {
			summary = "Commit " + strings.TrimSpace(summary[len("commit "):])
		}
		if summary == "" && e.FilePath != "" {
			if e.FileOp != "" {
				summary = fmt.Sprintf("%s (%s)", e.FilePath, e.FileOp)
			} else {
				summary = e.FilePath
			}
		}
		if summary == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%-4s  %-16s %s",
			service.RelativeTime(e.Timestamp),
			repo,
			summary))
	}
	return lines
}
