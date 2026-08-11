package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkOpen measures opening and closing an up-to-date database.
func BenchmarkOpen(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "lineage.db")
	if err := MigratePath(ctx, dbPath); err != nil {
		b.Fatal(err)
	}
	opts := DefaultOpenOptions()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h, err := Open(ctx, dbPath, opts)
		if err != nil {
			b.Fatal(err)
		}
		_ = Close(h)
	}
}

// BenchmarkOpenStages measures the main steps of opening a database.
func BenchmarkOpenStages(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "lineage.db")
	if err := MigratePath(ctx, dbPath); err != nil {
		b.Fatal(err)
	}
	opts := DefaultOpenOptions()
	var connect, pragmas, migrated, query time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		db, err := sql.Open("sqlite", sqliteDSN(dbPath, opts))
		if err != nil {
			b.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if err := db.PingContext(ctx); err != nil {
			b.Fatal(err)
		}
		t1 := time.Now()
		if err := applyPragmas(ctx, db, opts); err != nil {
			b.Fatal(err)
		}
		t2 := time.Now()
		if err := migrateDB(ctx, db); err != nil {
			b.Fatal(err)
		}
		t3 := time.Now()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM repositories").Scan(&n); err != nil {
			b.Fatal(err)
		}
		t4 := time.Now()
		connect += t1.Sub(t0)
		pragmas += t2.Sub(t1)
		migrated += t3.Sub(t2)
		query += t4.Sub(t3)
		_ = db.Close()
	}
	b.StopTimer()
	n := int64(b.N)
	b.ReportMetric(float64(connect.Microseconds())/float64(n), "connect-us/op")
	b.ReportMetric(float64(pragmas.Microseconds())/float64(n), "pragmas-us/op")
	b.ReportMetric(float64(migrated.Microseconds())/float64(n), "migrate-us/op")
	b.ReportMetric(float64(query.Microseconds())/float64(n), "query-us/op")
}
