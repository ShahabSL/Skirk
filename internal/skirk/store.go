package skirk

import "context"

type ObjectInfo struct {
	Name    string
	ID      string
	Size    int64
	Updated string
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
