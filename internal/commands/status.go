package commands

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/semanticash/cli/internal/version"
	"github.com/spf13/cobra"
)

func NewStatusCmd(rootOpts *RootOptions) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show AI activity overview for this repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewStatusService()
			res, err := svc.Status(cmd.Context(), service.StatusInput{
				RepoPath: rootOpts.RepoPath,
			})
			if err != nil {
				return err
			}

			authState := auth.GetAuthState()
			res.WorkspaceTierTitle = lookupWorkspaceTierTitle(cmd.Context())
			if update := lookupCLIUpdate(cmd.Context()); update != nil {
				res.UpdateAvailable = true
				res.LatestVersion = update.LatestVersion
				res.UpdateDownloadURL = update.DownloadURL
			}

			out := cmd.OutOrStdout()

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			if isTerminalWriter(out) {
				_, _ = lipgloss.Fprintln(out, renderStatusCard(res, authState))
				return nil
			}

			_, _ = fmt.Fprint(out, renderStatusPlain(res, authState))

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")

	return cmd
}

func renderStatusPlain(res *service.StatusResult, authState auth.AuthState) string {
	view := buildStatusView(res, authState)
	var b strings.Builder

	if !view.Enabled {
		b.WriteString("Semantica: not enabled\n")
		b.WriteString(renderStatusFieldPlain(statusField{Label: "Authenticated", Value: view.Authenticated}) + "\n")
		if view.WorkspaceTier != "" {
			b.WriteString(renderStatusFieldPlain(statusField{Label: "Workspace tier", Value: view.WorkspaceTier}) + "\n")
		}
		b.WriteString(view.Hint + "\n")
		if view.UpgradeVersion != "" {
			b.WriteString("\n")
			b.WriteString(renderUpgradePlain(view.UpgradeVersion))
		}
		return b.String()
	}

	b.WriteString("Semantica: enabled\n")
	b.WriteString("Repository: " + view.Repository + "\n")
	b.WriteString(renderStatusFieldPlain(statusField{Label: "Authenticated", Value: view.Authenticated}) + "\n")
	if view.WorkspaceTier != "" {
		b.WriteString(renderStatusFieldPlain(statusField{Label: "Workspace tier", Value: view.WorkspaceTier}) + "\n")
	}
	b.WriteString(renderStatusFieldPlain(statusField{Label: "Connected", Value: view.Connected}) + "\n")
	b.WriteString(renderStatusFieldPlain(statusField{Label: "Endpoint", Value: view.Endpoint}) + "\n")
	b.WriteString(renderStatusSectionsPlain(view.Sections))

	if view.Hint != "" {
		b.WriteString("\n" + view.Hint + "\n")
	}

	if view.UpgradeVersion != "" {
		b.WriteString("\n")
		b.WriteString(renderUpgradePlain(view.UpgradeVersion))
	}

	return b.String()
}

func renderStatusCard(res *service.StatusResult, authState auth.AuthState) string {
	view := buildStatusView(res, authState)
	theme := enableCardTheme()

	boxStyle := theme.Focused.Card.
		UnsetBorderLeft().
		BorderStyle(lipgloss.NormalBorder()).
		BorderTop(true).
		BorderRight(true).
		BorderBottom(true).
		BorderLeft(true).
		Padding(0, 1)
	labelStyle := lipgloss.NewStyle().Bold(true)
	valueStyle := lipgloss.NewStyle()
	subtleStyle := theme.Focused.Description
	titleStyle := theme.Focused.SelectedOption.Bold(true)
	sectionStyle := labelStyle
	sectionBodyStyle := lipgloss.NewStyle().PaddingLeft(2)

	header := lipgloss.JoinHorizontal(lipgloss.Center,
		titleStyle.Render("Semantica"),
		" ",
		subtleStyle.Render(version.Version),
	)

	sections := []string{
		header,
		subtleStyle.Render("Code, with provenance."),
		"",
		lipgloss.JoinVertical(lipgloss.Left, renderStatusFieldsCard(labelStyle, valueStyle, view.Overview)...),
	}

	if view.Hint != "" {
		sections = append(sections, "", subtleStyle.Render(view.Hint))
	}
	for _, section := range view.Sections {
		sections = append(sections, "", renderStatusSectionCard(sectionStyle, sectionBodyStyle, labelStyle, valueStyle, section))
	}

	if upgradeCard := renderUpgradeCard(theme, view.UpgradeVersion != "", view.UpgradeVersion); upgradeCard != "" {
		sections = append(sections, "", upgradeCard)
	}

	return boxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, sections...))
}

type statusView struct {
	Enabled        bool
	Repository     string
	Authenticated  string
	WorkspaceTier  string
	Connected      string
	Endpoint       string
	Overview       []statusField
	Sections       []statusSection
	Hint           string
	UpgradeVersion string
}

type statusField struct {
	Label string
	Value string
}

type statusSection struct {
	Title  string
	Value  string
	Fields []statusField
	Lines  []string
}

func buildStatusView(res *service.StatusResult, authState auth.AuthState) statusView {
	view := statusView{
		Enabled:        res.Enabled,
		Repository:     res.RepoRoot,
		Authenticated:  authStateValue(authState),
		WorkspaceTier:  res.WorkspaceTierTitle,
		Connected:      statusConnectedValue(res),
		Endpoint:       res.Endpoint,
		Hint:           statusHint(res),
		UpgradeVersion: statusUpgradeVersion(res),
	}
	view.Overview = statusOverviewFields(res, view)
	view.Sections = statusSections(res)
	return view
}

func statusOverviewFields(res *service.StatusResult, view statusView) []statusField {
	fields := []statusField{
		{Label: "Status", Value: statusHeaderValue(res)},
		{Label: "Authenticated", Value: view.Authenticated},
	}
	if res.Enabled {
		fields = append(fields, statusField{Label: "Store", Value: filepath.Join(res.RepoRoot, ".semantica")})
	}
	if view.WorkspaceTier != "" {
		fields = append(fields, statusField{Label: "Workspace tier", Value: view.WorkspaceTier})
	}
	if res.Enabled {
		fields = append(fields,
			statusField{Label: "Connected", Value: view.Connected},
			statusField{Label: "Endpoint", Value: view.Endpoint},
		)
	}
	return fields
}

func statusSettingsFields(res *service.StatusResult) []statusField {
	fields := make([]statusField, 0, 4)
	if res.RepoProvider != "unknown" {
		fields = append(fields, statusField{Label: "Remote", Value: res.RepoProvider})
	}
	fields = append(fields, statusField{Label: "Auto-playbook", Value: enabledValue(res.AutoPlaybook)})
	fields = append(fields, statusField{Label: "Git Trailers", Value: enabledValue(res.GitTrailers)})
	if len(res.Providers) > 0 {
		fields = append(fields, statusField{Label: "Agents", Value: strings.Join(res.Providers, ", ")})
	}
	return fields
}

func statusSections(res *service.StatusResult) []statusSection {
	if !res.Enabled {
		return nil
	}

	sections := []statusSection{
		{Title: "Settings", Fields: statusSettingsFields(res)},
	}

	if res.LastCheckpoint != nil {
		sections = append(sections, statusSection{
			Title: "Last checkpoint",
			Lines: []string{formatCheckpointLine(res.LastCheckpoint)},
		})
	}

	if len(res.RecentSessions) > 0 {
		lines := make([]string, 0, len(res.RecentSessions))
		for _, s := range res.RecentSessions {
			lines = append(lines, formatRecentSessionLine(s))
		}
		sections = append(sections, statusSection{
			Title: "Recent sessions (last 24h)",
			Lines: lines,
		})
	}

	if len(res.AITrend) > 0 {
		lines := make([]string, 0, len(res.AITrend))
		for _, t := range res.AITrend {
			lines = append(lines, formatTrendLine(t))
		}
		sections = append(sections, statusSection{
			Title: "AI attribution trend",
			Lines: lines,
		})
	}

	if res.PlaybookCount > 0 {
		sections = append(sections, statusSection{
			Title: "Playbooks",
			Value: fmt.Sprintf("%d", res.PlaybookCount),
		})
	}

	if res.Broker != nil {
		lines := make([]string, 0, len(res.Broker.Repos))
		for _, r := range res.Broker.Repos {
			lines = append(lines, formatBrokerLine(r))
		}
		sections = append(sections, statusSection{
			Title: "Broker monitoring",
			Value: brokerSummary(res.Broker),
			Lines: lines,
		})
	}

	return sections
}

func statusHint(res *service.StatusResult) string {
	if !res.Enabled {
		return "Run `semantica enable` to get started."
	}
	if res.LastCheckpoint == nil && len(res.RecentSessions) == 0 {
		return "No checkpoints yet. Commit with an AI agent to start tracking."
	}
	return ""
}

func statusUpgradeVersion(res *service.StatusResult) string {
	if !res.UpdateAvailable || res.LatestVersion == "" {
		return ""
	}
	return res.LatestVersion
}

func renderStatusFieldPlain(field statusField) string {
	return field.Label + ": " + field.Value
}

func renderStatusFieldsCard(labelStyle, valueStyle lipgloss.Style, fields []statusField) []string {
	lines := make([]string, 0, len(fields))
	for _, field := range fields {
		lines = append(lines, enableCardRow(labelStyle, valueStyle, field.Label, field.Value))
	}
	return lines
}

func renderStatusSectionsPlain(sections []statusSection) string {
	var b strings.Builder
	for _, section := range sections {
		b.WriteString("\n")
		b.WriteString(renderStatusSectionPlain(section))
	}
	return b.String()
}

func renderStatusSectionPlain(section statusSection) string {
	var b strings.Builder

	if section.Value != "" && len(section.Fields) == 0 && len(section.Lines) == 0 {
		b.WriteString(section.Title + ": " + section.Value + "\n")
		return b.String()
	}

	if section.Value != "" {
		b.WriteString(section.Title + ": " + section.Value + "\n")
	} else {
		b.WriteString(section.Title + "\n")
	}
	for _, field := range section.Fields {
		b.WriteString("  " + renderStatusFieldPlain(field) + "\n")
	}
	for _, line := range section.Lines {
		b.WriteString("  " + line + "\n")
	}

	return b.String()
}

func renderStatusSectionCard(sectionStyle, sectionBodyStyle, labelStyle, valueStyle lipgloss.Style, section statusSection) string {
	if section.Value != "" && len(section.Fields) == 0 && len(section.Lines) == 0 {
		return enableCardRow(labelStyle, valueStyle, section.Title, section.Value)
	}

	lines := []string{sectionStyle.Render(section.Title)}
	if section.Value != "" {
		lines[0] = enableCardRow(labelStyle, valueStyle, section.Title, section.Value)
	}
	lines = append(lines, renderStatusSectionBodyCard(sectionBodyStyle, labelStyle, valueStyle, section)...)
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

func renderStatusSectionBodyCard(sectionBodyStyle, labelStyle, valueStyle lipgloss.Style, section statusSection) []string {
	lines := make([]string, 0, len(section.Fields)+len(section.Lines))
	for _, field := range section.Fields {
		lines = append(lines, sectionBodyStyle.Render(enableCardRow(labelStyle, valueStyle, field.Label, field.Value)))
	}
	for _, line := range section.Lines {
		lines = append(lines, sectionBodyStyle.Render(valueStyle.Render(line)))
	}
	return lines
}

func authStateValue(s auth.AuthState) string {
	if s.StorageError != "" {
		return "no (credential storage error: " + s.StorageError + ")"
	}
	if !s.Authenticated {
		return "no"
	}
	if s.Source == "api_key" {
		return "yes (API key)"
	}
	if s.Email != "" {
		return "yes (" + s.Email + ")"
	}
	return "yes"
}

func statusHeaderValue(res *service.StatusResult) string {
	if !res.Enabled {
		return "Not enabled"
	}
	return "Enabled in " + res.RepoRoot
}

func statusConnectedValue(res *service.StatusResult) string {
	if res.Connected {
		return "yes"
	}
	if !res.HasRemote {
		return "no (no git remote found for repository)"
	}
	if res.RepoProvider == "unknown" {
		return "no (unsupported host)"
	}
	return "no"
}

func enabledValue(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}

func formatCheckpointLine(cp *service.LastCheckpointInfo) string {
	commit := ""
	if cp.Commit != "" {
		commit = "  commit " + util.ShortID(cp.Commit)
	}

	msg := ""
	if cp.Message != "" && !strings.HasPrefix(cp.Message, "Auto checkpoint") {
		if len(cp.Message) > 60 {
			msg = "  \"" + cp.Message[:57] + "...\""
		} else {
			msg = "  \"" + cp.Message + "\""
		}
	}

	return fmt.Sprintf("%s  %s ago  %s%s%s",
		util.ShortID(cp.ID),
		service.RelativeTime(cp.CreatedAt),
		cp.Kind,
		commit,
		msg,
	)
}

func formatRecentSessionLine(s service.RecentSessionInfo) string {
	duration := service.FormatDuration(s.StartedAt, s.LastEventAt)
	tok := fmt.Sprintf("tok %s/%s",
		service.CompactTokens(s.TokensIn),
		service.CompactTokens(s.TokensOut))
	if s.TokensCached > 0 {
		tok += fmt.Sprintf(" (+%s cached)", service.CompactTokens(s.TokensCached))
	}
	return fmt.Sprintf("%s  %-12s  %s ago  duration %s  steps %d  %s",
		util.ShortID(s.SessionID),
		s.Provider,
		service.RelativeTime(s.LastEventAt),
		duration,
		s.StepCount,
		tok,
	)
}

func formatTrendLine(t service.AITrendPoint) string {
	commit := t.CommitHash
	if len(commit) > 8 {
		commit = commit[:8]
	}
	return fmt.Sprintf("%s  %5.1f%%  %s ago",
		commit,
		t.AIPercentage,
		service.RelativeTime(t.CreatedAt),
	)
}

func brokerSummary(b *service.BrokerStatusInfo) string {
	if b.InactiveRepos > 0 {
		return fmt.Sprintf("%d active, %d inactive", b.ActiveRepos, b.InactiveRepos)
	}
	return fmt.Sprintf("%d active", b.ActiveRepos)
}

func formatBrokerLine(r broker.RepoInfo) string {
	state := "active"
	if !r.Active {
		state = "inactive"
	}
	return fmt.Sprintf("%-8s  %s  enabled %s ago",
		state,
		r.Path,
		service.RelativeTime(r.EnabledAt),
	)
}
