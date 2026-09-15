// Package turncapture stores local turn observations independently of attribution.
package turncapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/toolsnap"
)

type Subject struct {
	RepositoryID   string `json:"repository_id,omitempty"`
	RegistrationID string `json:"registration_id"`
	Path           string `json:"path"`
	Gap            string `json:"gap,omitempty"`
}

type RepoObservation struct {
	Subject  Subject               `json:"subject"`
	Baseline toolsnap.TurnBaseline `json:"baseline"`
	Gap      string                `json:"gap,omitempty"`
}

// Evidence records a signal received by the hook adapter.
type Evidence struct {
	Kind        string          `json:"kind"`
	ExecutionID string          `json:"execution_id,omitempty"`
	TaskID      string          `json:"task_id,omitempty"`
	Status      string          `json:"status,omitempty"`
	ReceivedAt  time.Time       `json:"received_at"`
	ProviderAt  int64           `json:"provider_at,omitempty"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

// End records observations and completion evidence at the first end boundary.
type End struct {
	BoundaryAt        time.Time                  `json:"boundary_at"`
	FinishedAt        time.Time                  `json:"finished_at"`
	TrackedCompletion string                     `json:"tracked_completion_at_end"`
	CompletionReason  string                     `json:"completion_reason"`
	Evidence          []Evidence                 `json:"provider_evidence_at_end"`
	Repositories      []toolsnap.TurnObservation `json:"repositories"`
}

type Record struct {
	Version            int               `json:"version"`
	Provider           string            `json:"provider"`
	SessionID          string            `json:"session_id"`
	TurnID             string            `json:"turn_id"`
	ProviderTurnID     string            `json:"provider_turn_id,omitempty"`
	BoundaryKey        string            `json:"boundary_key"`
	StartedAt          time.Time         `json:"started_at"`
	BaselineFinishedAt time.Time         `json:"baseline_finished_at"`
	Repositories       []RepoObservation `json:"repositories"`
	Evidence           []Evidence        `json:"evidence"`
	End                *End              `json:"end,omitempty"`
}

type session struct {
	Current    string               `json:"current"`
	Owners     map[string]string    `json:"execution_owners"`
	TerminalAt map[string]time.Time `json:"terminal_at"`
}

type Recorder struct{ Root string }

func identity(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (r Recorder) dir(provider, sessionID string) string {
	return filepath.Join(r.Root, identity(provider+"\x00"+sessionID))
}

func read(path string, into any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func save(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := platform.WriteFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	return platform.SyncDir(filepath.Dir(path))
}

func (r Recorder) locked(ctx context.Context, provider, sessionID string, fn func(string, *session) error) error {
	if provider == "" || sessionID == "" {
		return fmt.Errorf("turn identity missing")
	}
	dir := r.dir(provider, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return platform.WithFileLock(ctx, filepath.Join(dir, "session.json"), func() error {
		s := session{Owners: make(map[string]string)}
		if err := read(filepath.Join(dir, "session.json"), &s); err != nil && !os.IsNotExist(err) {
			return err
		}
		if s.Owners == nil {
			s.Owners = make(map[string]string)
		}
		if s.TerminalAt == nil {
			s.TerminalAt = make(map[string]time.Time)
		}
		if s.Current != "" && !recordKey(s.Current) {
			return fmt.Errorf("invalid current turn key")
		}
		for _, key := range s.Owners {
			if !recordKey(key) {
				return fmt.Errorf("invalid execution owner")
			}
		}
		return fn(dir, &s)
	})
}

// Begin saves the repository set before capture. Retries reuse the saved baseline.
func (r Recorder) Begin(ctx context.Context, provider, sessionID, turnID, providerTurnID, boundaryKey string, subjects []Subject) error {
	if turnID == "" || boundaryKey == "" {
		return fmt.Errorf("turn boundary missing")
	}
	return r.locked(ctx, provider, sessionID, func(dir string, s *session) error {
		key := identity(boundaryKey)
		path := filepath.Join(dir, key+".json")
		var old Record
		if err := read(path, &old); err == nil {
			if old.Version != 1 || old.Provider != provider || old.SessionID != sessionID || old.BoundaryKey != boundaryKey {
				return fmt.Errorf("turn record identity mismatch")
			}
			// Restore the cursor if saving it was interrupted.
			var current Record
			if s.Current != "" {
				if err := read(filepath.Join(dir, s.Current+".json"), &current); err != nil {
					return err
				}
			}
			if s.Current == "" || old.StartedAt.After(current.StartedAt) {
				s.Current = key
				return save(filepath.Join(dir, "session.json"), s)
			}
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		rec := Record{Version: 1, Provider: provider, SessionID: sessionID, TurnID: turnID, ProviderTurnID: providerTurnID, BoundaryKey: boundaryKey, StartedAt: time.Now().UTC()}
		seen := make(map[string]bool)
		for _, subject := range subjects {
			if !filepath.IsAbs(subject.Path) || seen[subject.Path] {
				return fmt.Errorf("invalid or duplicate turn subject")
			}
			seen[subject.Path] = true
			rec.Repositories = append(rec.Repositories, RepoObservation{Subject: subject, Gap: "baseline_interrupted"})
		}
		if err := save(path, rec); err != nil {
			return err
		}
		s.Current = key
		if err := save(filepath.Join(dir, "session.json"), s); err != nil {
			return err
		}
		parallel(len(subjects), func(i int) {
			v := &rec.Repositories[i]
			if v.Subject.Gap != "" {
				v.Gap = v.Subject.Gap
				return
			}
			var err error
			v.Baseline, err = toolsnap.FreezeTurnSubject(ctx, v.Subject.Path)
			v.Gap = ""
			if err != nil {
				v.Gap = err.Error()
			}
		})
		parallel(len(subjects), func(i int) {
			v := &rec.Repositories[i]
			if v.Gap != "" {
				return
			}
			if err := toolsnap.CaptureTurnBaseline(ctx, storePath(dir, key, i), &v.Baseline); err != nil {
				v.Gap = err.Error()
			}
		})
		rec.BaselineFinishedAt = time.Now().UTC()
		return save(path, rec)
	})
}

func storePath(dir, key string, i int) string { return filepath.Join(dir, key, fmt.Sprint(i)) }

func recordKey(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

func parallel(count int, fn func(int)) {
	var wg sync.WaitGroup
	workers := min(count, 8)
	for n := 0; n < workers; n++ {
		wg.Go(func() {
			for i := n; i < count; i += workers {
				fn(i)
			}
		})
	}
	wg.Wait()
}

// Observe appends hook evidence and records Stop before capturing end state.
// Retries never replace an interrupted capture with a later snapshot.
func (r Recorder) Observe(ctx context.Context, provider, sessionID, providerTurnID string, evidence []Evidence, stop bool) error {
	if _, err := os.Stat(filepath.Join(r.dir(provider, sessionID), "session.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return r.locked(ctx, provider, sessionID, func(dir string, s *session) error {
		key := s.Current
		if providerTurnID != "" {
			key = identity("provider:" + providerTurnID)
		}
		if len(evidence) == 1 && evidence[0].ExecutionID != "" {
			if owner := s.Owners[evidence[0].ExecutionID]; owner != "" {
				key = owner
			}
		}
		if key == "" {
			return nil
		}
		path := filepath.Join(dir, key+".json")
		var rec Record
		if err := read(path, &rec); err != nil {
			return err
		}
		if rec.Version != 1 || rec.Provider != provider || rec.SessionID != sessionID || identity(rec.BoundaryKey) != key {
			return fmt.Errorf("turn record identity mismatch")
		}
		for _, e := range evidence {
			rec.Evidence = append(rec.Evidence, e)
			if e.Kind == "execution_started" && e.ExecutionID != "" {
				s.Owners[e.ExecutionID] = key
			}
			if e.Kind == "execution_terminal" && e.ExecutionID != "" {
				s.TerminalAt[e.ExecutionID] = e.ReceivedAt
			}
		}
		if err := save(path, rec); err != nil {
			return err
		}
		if err := save(filepath.Join(dir, "session.json"), s); err != nil {
			return err
		}
		if !stop || rec.End != nil {
			return nil
		}
		boundary := time.Now().UTC()
		for _, e := range evidence {
			if e.Kind == "stop" {
				boundary = e.ReceivedAt
				break
			}
		}
		atEnd := make([]Evidence, 0, len(rec.Evidence))
		for _, e := range rec.Evidence {
			if !e.ReceivedAt.After(boundary) {
				atEnd = append(atEnd, e)
			}
		}
		// Include unfinished work from earlier turns.
		owners := make(map[string]bool)
		for id, owner := range s.Owners {
			terminal := s.TerminalAt[id]
			if owner != key && (terminal.IsZero() || terminal.After(boundary)) {
				owners[owner] = true
			}
		}
		for owner := range owners {
			var earlier Record
			if err := read(filepath.Join(dir, owner+".json"), &earlier); err != nil {
				atEnd = append(atEnd, Evidence{Kind: "gap", Status: "earlier_execution_evidence_unavailable", ReceivedAt: boundary})
				continue
			}
			for _, e := range earlier.Evidence {
				if e.Kind != "stop" && e.Kind != "inventory_running" && !e.ReceivedAt.After(boundary) {
					atEnd = append(atEnd, e)
				}
			}
		}
		completion, reason := Completion(atEnd)
		rec.End = &End{BoundaryAt: boundary, TrackedCompletion: completion, CompletionReason: reason, Evidence: atEnd}
		for range rec.Repositories {
			rec.End.Repositories = append(rec.End.Repositories, toolsnap.TurnObservation{State: "unknown", Reason: "end_observation_interrupted"})
		}
		if err := save(path, rec); err != nil {
			return err
		}
		parallel(len(rec.Repositories), func(i int) {
			v := rec.Repositories[i]
			if rec.BaselineFinishedAt.IsZero() || rec.BaselineFinishedAt.After(boundary) {
				rec.End.Repositories[i].Reason = "baseline_boundary_untrusted"
				return
			}
			if v.Gap != "" {
				rec.End.Repositories[i].Reason = v.Gap
				return
			}
			rec.End.Repositories[i] = toolsnap.ObserveTurnEnd(ctx, storePath(dir, key, i), v.Baseline)
		})
		rec.End.FinishedAt = time.Now().UTC()
		return save(path, rec)
	})
}

// Completion assesses known provider-tracked work, not detached descendants.
func Completion(evidence []Evidence) (string, string) {
	started, terminal, tasks := map[string]bool{}, map[string]bool{}, map[string]bool{}
	unknown, running := false, false
	for _, e := range evidence {
		switch e.Kind {
		case "execution_started":
			if e.ExecutionID == "" {
				unknown = true
			} else {
				started[e.ExecutionID] = true
			}
		case "execution_terminal":
			if e.ExecutionID == "" {
				unknown = true
			} else {
				terminal[e.ExecutionID] = true
			}
		case "managed_task":
			terminal[e.ExecutionID] = true
			tasks[e.TaskID] = true
		case "inventory_running":
			running = true
		case "gap":
			unknown = true
		}
	}
	if running {
		return "unsettled", "provider_reports_running_work"
	}
	if len(tasks) > 0 {
		return "unknown", "managed_task_terminal_source_unavailable"
	}
	for id := range started {
		if !terminal[id] {
			unknown = true
		}
	}
	for id := range terminal {
		if !started[id] {
			unknown = true
		}
	}
	if unknown {
		return "unknown", "execution_evidence_incomplete"
	}
	if len(started) == 0 {
		return "unknown", "no_tracked_execution_evidence"
	}
	return "settled", "known_tracked_scopes_terminal"
}
