package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureGroupIndexRepair(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "existing"
		if missing {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := openRaw(t, filepath.Join(t.TempDir(), "lineage.db"))
			if err := migrateWithSource(db, schemaAt(t, 10), 10); err != nil {
				t.Fatal(err)
			}
			if missing {
				if _, err := db.Exec("drop index idx_event_evidence_links_group"); err != nil {
					t.Fatal(err)
				}
				if exists, err := dirtyProbes[11](ctx, db); err != nil || exists {
					t.Fatalf("missing probe: %v %v", exists, err)
				}
			}
			if err := migrateDB(ctx, db); err != nil {
				t.Fatal(err)
			}
			if exists, err := dirtyProbes[11](ctx, db); err != nil || !exists {
				t.Fatalf("applied probe: %v %v", exists, err)
			}
			query, err := os.ReadFile("queries/capture_group.sql")
			if err != nil {
				t.Fatal(err)
			}
			rows, err := db.Query("explain query plan "+string(query), "group", "repo")
			if err != nil {
				t.Fatal(err)
			}
			usesIndex := false
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				usesIndex = usesIndex || strings.Contains(detail, "idx_event_evidence_links_group")
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if !usesIndex {
				t.Fatal("group query does not use the index")
			}
			if err := migrateWithSource(db, schemaAt(t, 11), 10); err != nil {
				t.Fatal(err)
			}
			if exists, err := dirtyProbes[11](ctx, db); err != nil || !exists {
				t.Fatalf("rollback removed the migration-6 index: %v %v", exists, err)
			}
			if err := migrateDB(ctx, db); err != nil {
				t.Fatal(err)
			}
		})
	}
}
