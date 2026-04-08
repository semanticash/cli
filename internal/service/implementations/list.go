package implementations

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

// ListInput controls which implementations are returned.
type ListInput struct {
	Limit         int64
	All           bool // include old dormant, closed, and single-repo
	IncludeSingle bool // include single-repo implementations
}

// ListItem is one row in the implementations listing.
type ListItem struct {
	ImplementationID string        `json:"implementation_id"`
	Title            string        `json:"title,omitempty"`
	State            string        `json:"state"`
	RepoCount        int64         `json:"repo_count"`
	CommitCount      int64         `json:"commit_count"`
	LastActivityAt   int64         `json:"last_activity_at"`
	Repos            []RepoSummary `json:"repos"`
}

// RepoSummary is a lightweight repo reference for the list view.
type RepoSummary struct {
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// ListResult wraps the list output.
type ListResult struct {
	Items []ListItem `json:"items"`
	Total int        `json:"total"`
}

// implRow is the common shape across all list query row types.
type implRow struct {
	ImplementationID string
	Title            sql.NullString
	State            string
	RepoCount        int64
	CommitCount      int64
	LastActivityAt   int64
}

// List returns implementations matching the filter criteria.
func List(ctx context.Context, in ListInput) (*ListResult, error) {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return &ListResult{}, nil // no DB yet -> empty list
	}
	defer func() { _ = impldb.Close(h) }()

	if in.Limit <= 0 {
		in.Limit = 20
	}

	var common []implRow
	if in.All {
		rows, qerr := h.Queries.ListAllImplementations(ctx, in.Limit)
		if qerr != nil {
			return nil, fmt.Errorf("list implementations: %w", qerr)
		}
		for _, r := range rows {
			common = append(common, implRow{r.ImplementationID, r.Title, r.State, r.RepoCount, r.CommitCount, r.LastActivityAt})
		}
	} else if in.IncludeSingle {
		rows, qerr := h.Queries.ListImplementationsByState(ctx, impldbgen.ListImplementationsByStateParams{
			States: []string{"active", "dormant"},
			Limit:  in.Limit,
		})
		if qerr != nil {
			return nil, fmt.Errorf("list implementations: %w", qerr)
		}
		for _, r := range rows {
			common = append(common, implRow{r.ImplementationID, r.Title, r.State, r.RepoCount, r.CommitCount, r.LastActivityAt})
		}
	} else {
		rows, qerr := h.Queries.ListActiveOrMultiRepo(ctx, in.Limit)
		if qerr != nil {
			return nil, fmt.Errorf("list implementations: %w", qerr)
		}
		for _, r := range rows {
			common = append(common, implRow{r.ImplementationID, r.Title, r.State, r.RepoCount, r.CommitCount, r.LastActivityAt})
		}
	}

	items := make([]ListItem, 0, len(common))
	for _, r := range common {
		repos, _ := h.Queries.ListImplementationRepos(ctx, r.ImplementationID)
		repoSummaries := repoSummariesFromRows(repos)

		title := ""
		if r.Title.Valid {
			title = r.Title.String
		}

		items = append(items, ListItem{
			ImplementationID: r.ImplementationID,
			Title:            title,
			State:            r.State,
			RepoCount:        r.RepoCount,
			CommitCount:      r.CommitCount,
			LastActivityAt:   r.LastActivityAt,
			Repos:            repoSummaries,
		})
	}

	return &ListResult{Items: items, Total: len(items)}, nil
}

func repoSummariesFromRows(repos []impldbgen.ImplementationRepo) []RepoSummary {
	out := make([]RepoSummary, 0, len(repos))
	for _, rr := range repos {
		s := RepoSummary{DisplayName: rr.DisplayName, Role: rr.RepoRole}
		if rr.RepoRole == "origin" {
			out = append([]RepoSummary{s}, out...) // origin first
		} else {
			out = append(out, s)
		}
	}
	return out
}

// openGlobalDB opens the global implementations database.
func openGlobalDB(ctx context.Context) (*impldb.Handle, error) {
	base, err := broker.GlobalBase()
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(base, "implementations.db")
	return impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
}
