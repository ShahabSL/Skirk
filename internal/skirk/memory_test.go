package skirk

import (
	"context"
	"strings"
	"testing"
)

func TestMemoryStorePutObjectWithIDIsIdempotent(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	info, err := store.PutObjectWithID(ctx, "id-1", "name", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.PutObjectWithID(ctx, "id-1", "name", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if retry != info {
		t.Fatalf("retry info = %+v, want %+v", retry, info)
	}
	got, err := store.GetByID(ctx, "id-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Fatalf("payload = %q", got)
	}
}

func TestMemoryStorePutObjectWithIDRejectsConflicts(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if _, err := store.PutObjectWithID(ctx, "id-1", "name", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutObjectWithID(ctx, "id-1", "other", []byte("payload")); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("same id different name err = %v, want name conflict", err)
	}
	if _, err := store.PutObjectWithID(ctx, "id-1", "name", []byte("other")); err == nil || !strings.Contains(err.Error(), "data") {
		t.Fatalf("same id different data err = %v, want data conflict", err)
	}
	if _, err := store.PutObjectWithID(ctx, "id-2", "name", []byte("payload")); err == nil || !strings.Contains(err.Error(), "name conflict") {
		t.Fatalf("same name different id err = %v, want name conflict", err)
	}
}
