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

type ObjectRangeInfo struct {
	Start int64
	End   int64
	Total int64
}

type ChangeInfo struct {
	ID      string
	FileID  string
	Name    string
	Size    int64
	Updated string
	Removed bool
}

type ChangeListInfo struct {
	Changes           []ChangeInfo
	NextPageToken     string
	NewStartPageToken string
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

type ObjectPutIDStore interface {
	PutObjectWithID(ctx context.Context, fileID, name string, data []byte) (ObjectInfo, error)
}

type ObjectIDReserveStore interface {
	GenerateObjectIDs(ctx context.Context, count int) ([]string, error)
}

type ObjectIDStore interface {
	GetByID(ctx context.Context, fileID string) ([]byte, error)
	DeleteID(ctx context.Context, fileID string) error
}

type RangeObjectStore interface {
	GetObjectRangeByID(ctx context.Context, fileID string, start, end int64) ([]byte, ObjectRangeInfo, error)
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

type ChangeFeedStore interface {
	ChangesStartPageToken(ctx context.Context) (string, error)
	ListChanges(ctx context.Context, pageToken string, includeRemoved bool) (ChangeListInfo, error)
}
