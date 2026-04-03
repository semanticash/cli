package api

import (
	"context"
)

type Source struct {
	Provider    string
	SourceKey   string
	ProjectPath string // optional: resolved project path, set by FilterSources
}

type BlobPutter interface {
	Put(ctx context.Context, b []byte) (hash string, size int64, err error)
}
