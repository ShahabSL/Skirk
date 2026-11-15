package skirk

import (
	"context"
	"time"
)

type ObjectInfo struct {
	Name       string
	ID         string
	Size       int64
	Updated    string
	InlineData []byte // Optional encrypted object bytes from discovery metadata.
}

// MetadataOnlyPriorityStore can place a small sealed priority object entirely
// in Drive metadata. The caller must negotiate reader support before use.
type MetadataOnlyPriorityStore interface {
	MetadataOnlyPriorityEnabled() bool
	PutMetadataOnlyObject(ctx context.Context, name string, sealed []byte) (ObjectInfo, error)
}

// MetadataSlotInfo is the exact file metadata needed by an authenticated
// fixed-slot protocol. Version is only a change hint, not a delivery sequence.
type MetadataSlotInfo struct {
	ID          string
	Name        string
	Description string
	Version     uint64
	Parents     []string
	Spaces      []string
}

// MetadataSlotStore is an opt-in Drive primitive. The protocol owns single-
// writer PATCH serialization, readback after ambiguous writes, and cleanup.
type MetadataSlotStore interface {
	MetadataSlotEnabled() bool
	CreateMetadataSlot(ctx context.Context, generatedID, name string) (MetadataSlotInfo, error)
	GetMetadataSlot(ctx context.Context, fileID, expectedName string) (MetadataSlotInfo, error)
	PatchMetadataSlot(ctx context.Context, fileID, expectedName, description string) (MetadataSlotInfo, error)
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

// NormalObjectIDStore lets the mux isolate large normal media reads from its
// control HTTP pool without changing the path for priority or small objects.
type NormalObjectIDStore interface {
	GetNormalByID(ctx context.Context, fileID string) ([]byte, error)
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

type FreshListContainsPageStatusStore interface {
	ListFreshContainsPageStatus(ctx context.Context, contains []string, since time.Time, pageToken string, maxPages int) (ObjectListInfo, error)
}

type ChangeFeedStore interface {
	ChangesStartPageToken(ctx context.Context) (string, error)
	ListChanges(ctx context.Context, pageToken string, includeRemoved bool) (ChangeListInfo, error)
}
