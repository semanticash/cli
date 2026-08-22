package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/version"
	"github.com/spf13/cobra"
)

// storageSoakSchemaVersion identifies the JSON record format.
const storageSoakSchemaVersion = 1

// storageSoakRecord adds schema and build identity to one storage inspection.
type storageSoakRecord struct {
	SchemaVersion int    `json:"schema_version"`
	CLIVersion    string `json:"cli_version"`
	CLICommit     string `json:"cli_commit,omitempty"`
	toolsnap.StorageInspection
}

// storageSoakLogPath returns the repository-local JSONL log path.
func storageSoakLogPath(repoPath string) string {
	return filepath.Join(repoPath, ".semantica", "doctor", "attribution-v2-storage-soak.jsonl")
}

// inspectRepoStorage measures existing capture storage without modifying it.
func inspectRepoStorage(ctx context.Context, repoPath string, now time.Time) (toolsnap.StorageInspection, error) {
	semDir := filepath.Join(repoPath, ".semantica")
	rc, err := toolsnap.ResolveRepoContext(ctx, repoPath)
	if err != nil {
		return toolsnap.StorageInspection{}, err
	}
	store, err := toolsnap.OpenStoreForInspection(ctx, rc, semDir)
	if err != nil {
		return toolsnap.StorageInspection{}, err
	}
	reg, err := toolsnap.OpenRegistryForInspection(semDir)
	if err != nil {
		return toolsnap.StorageInspection{}, err
	}
	// grace 0 clamps to the default prune grace inside InspectStorage.
	return store.InspectStorage(ctx, reg, 0, now)
}

// appendStorageSoakLog durably appends one JSON line to the measurement log.
func appendStorageSoakLog(repoPath string, line []byte) (err error) {
	path := storageSoakLogPath(repoPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if _, err = f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// newDoctorStorageSoakCmd builds the read-only storage measurement command.
func newDoctorStorageSoakCmd(rootOpts *RootOptions) *cobra.Command {
	var logSeries bool

	cmd := &cobra.Command{
		Use:    "storage-soak",
		Short:  "Measure tool-snapshot store growth (read-only)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoPath := resolveDoctorRepo(rootOpts.RepoPath)
			if repoPath == "" {
				return fmt.Errorf("storage-soak: not inside a Git repository")
			}
			insp, err := inspectRepoStorage(cmd.Context(), repoPath, time.Now())
			if err != nil {
				return err
			}
			rec := storageSoakRecord{
				SchemaVersion:     storageSoakSchemaVersion,
				CLIVersion:        version.Version,
				CLICommit:         version.Commit,
				StorageInspection: insp,
			}
			// Use one encoded line for both stdout and the optional log entry.
			line, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if logSeries {
				if err := appendStorageSoakLog(repoPath, line); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(line))
			return err
		},
	}
	cmd.Flags().BoolVar(&logSeries, "log", false, "Append the measurement to .semantica/doctor/attribution-v2-storage-soak.jsonl")
	return cmd
}
