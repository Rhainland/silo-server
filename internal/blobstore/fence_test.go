package blobstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMutationFenceWaitsForRelease(t *testing.T) {
	base, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := WithMutationFence(base)
	release, err := store.(MutationFencer).BeginMutationFence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- store.Put(context.Background(), "tmdb/poster.webp", []byte("image")) }()

	select {
	case err := <-done:
		t.Fatalf("write passed an active fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("write did not resume after fence release")
	}
}

func TestMutationFenceBlockedWriteHonorsContext(t *testing.T) {
	base, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := WithMutationFence(base)
	release, err := store.(MutationFencer).BeginMutationFence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- store.Put(ctx, "tmdb/poster.webp", []byte("image")) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked write error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked write ignored context cancellation")
	}
}
