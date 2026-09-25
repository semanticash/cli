package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/turncapture"
)

// publishTurnObservation stores observed changes as repository context without authorship.
func publishTurnObservation(ctx context.Context, adapter HookProvider, event *Event, state *CaptureState, bh *broker.Handle) error {
	if state.TurnObservationKey == "" || bh == nil {
		return nil
	}
	provider := adapter.Name()
	lineageSession := lineageProviderSessionID(adapter, event, state)
	r, err := turnRecorder()
	if err != nil {
		return err
	}
	rec, err := r.ReadTurn(provider, turnCaptureSession(provider, event), state.TurnObservationKey, state.TurnID)
	if err != nil || rec == nil {
		return err
	}
	if event.ProviderTurnID != "" && event.ProviderTurnID != rec.ProviderTurnID {
		return fmt.Errorf("turn completion identity mismatch")
	}
	if rec.End == nil || rec.End.FinishedAt.IsZero() {
		return fmt.Errorf("turn observation is not complete")
	}
	if len(rec.Repositories) != len(rec.End.Repositories) {
		return fmt.Errorf("turn observation repository count mismatch")
	}
	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return err
	}
	active := make(map[string]broker.RegisteredRepo, len(repos))
	for _, repo := range repos {
		active[repo.CanonicalPath] = repo
	}
	// Direct publication must record the same launch root as routed events.
	launchRepoRoot := ""
	if r := broker.DeepestActiveRepo(state.CWD, repos); r != nil {
		launchRepoRoot = r.Path
	}
	for i, before := range rec.Repositories {
		after := rec.End.Repositories[i]
		if after.State != "changed" || before.Gap != "" || before.Subject.Gap != "" {
			continue
		}
		subject := before.Subject
		registered, ok := active[subject.Path]
		if !ok {
			continue // Skip repositories disabled since the baseline.
		}
		if registered.RepoID != subject.RegistrationID {
			return fmt.Errorf("turn destination registration changed")
		}
		id, err := broker.RepositoryIDForPath(ctx, subject.Path)
		if err != nil {
			return err
		}
		if id == "" || id != subject.RepositoryID {
			return fmt.Errorf("turn destination repository identity changed")
		}
		if err := toolsnap.ValidateTurnSubject(ctx, before.Baseline); err != nil {
			return err
		}
		// Exclude other repositories and evidence received after the end boundary.
		projected := *rec
		end := *rec.End
		projected.Evidence = nil
		projected.Repositories = []turncapture.RepoObservation{before}
		end.Repositories = []toolsnap.TurnObservation{after}
		projected.End = &end
		data, err := json.Marshal(projected)
		if err != nil {
			return err
		}
		bs, err := blobs.NewStore(filepath.Join(subject.Path, ".semantica", "objects"))
		if err != nil {
			return err
		}
		hash, _, err := bs.Put(ctx, data)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("turn-observation:%x", sha256.Sum256([]byte(provider+"\x00"+rec.SessionID+"\x00"+rec.BoundaryKey+"\x00"+subject.RepositoryID)))
		ev := broker.ObservationContext(broker.RawEvent{
			EventID: key, SourceKey: "turn-observation:" + rec.SessionID,
			Provider: provider, ProviderSessionID: lineageSession, TurnID: rec.TurnID,
			Timestamp: rec.End.BoundaryAt.UnixMilli(), SessionStartedAt: rec.StartedAt.UnixMilli(),
			Kind: "context", Role: "system", EventSource: "turn_observation",
			Summary:           "Repository changes observed during the turn; authorship not established.",
			SourceProjectPath: state.CWD,
			ResolvedRepoRoot:  launchRepoRoot,
		})
		if _, err := broker.WriteEventsToRepo(ctx, subject.Path, []broker.RawEvent{ev}, nil); err != nil {
			return err
		}
		links := []broker.EvidenceLink{{
			EventID: key, EvidenceKind: "turn_observation", EvidenceHash: hash,
			GroupID: key, CreatedAt: rec.End.BoundaryAt.UnixMilli(),
		}}
		// Link recorded commits regardless of when the observation is published.
		for _, change := range after.Changes {
			if change.Commit != "" && len(change.Files) > 0 {
				links = append(links, broker.EvidenceLink{
					EventID: key, EvidenceKind: "turn_observation", EvidenceHash: hash,
					GroupID: change.Commit, CreatedAt: rec.End.BoundaryAt.UnixMilli(),
				})
			}
		}
		if err := broker.WriteEvidenceLinksToRepo(ctx, subject.Path, links); err != nil {
			return err
		}
	}
	return nil
}
