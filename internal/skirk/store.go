package skirk

import (
	"context"
	"time"
)

type ObjectInfo struct {
	Name    string
	ID      string
	Size    int64
	Updated string
}

type ObjectListInfo struct {
	Objects       []ObjectInfo
	Truncated     bool
	NextPageToken string
	Pages         int
	Incomplete    bool
}

type BlobStore interface {
	Put(ctx context.Context, name string, data []byte) error
	Get(ctx context.Context, name string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	Delete(ctx context.Context, name string) error
}

type ObjectPutStore interface {
	PutObject(ctx context.Context, name string, data []byte) (ObjectInfo, error)
}

type ObjectIDStore interface {
	GetByID(ctx context.Context, fileID string) ([]byte, error)
	DeleteID(ctx context.Context, fileID string) error
}

type ContainsListStore interface {
	ListContains(ctx context.Context, contains []string) ([]ObjectInfo, error)
}

type FreshListStore interface {
	ListFresh(ctx context.Context, prefix string, since time.Time) ([]ObjectInfo, error)
}

type FreshListStatusStore interface {
	ListFreshStatus(ctx context.Context, prefix string, since time.Time) (ObjectListInfo, error)
}

type FreshListPageStatusStore interface {
	ListFreshPageStatus(ctx context.Context, prefix string, since time.Time, pageToken string) (ObjectListInfo, error)
}
