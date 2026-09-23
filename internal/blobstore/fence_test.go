package blobstore

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func TestMutationFenceWaitsForRelease(t *testing.T) {
	base, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		store := WithMutationFence(base)
		release, err := store.(MutationFencer).BeginMutationFence(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		done := make(chan error, 1)
		go func() { done <- store.Put(context.Background(), "tmdb/poster.webp", []byte("image")) }()

		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("write passed an active fence: %v", err)
		default:
		}
		release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestMutationFenceBlockedWriteHonorsContext(t *testing.T) {
	base, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		store := WithMutationFence(base)
		release, err := store.(MutationFencer).BeginMutationFence(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer release()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- store.Put(ctx, "tmdb/poster.webp", []byte("image")) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("write passed an active fence: %v", err)
		default:
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked write error = %v, want context.Canceled", err)
		}
	})
}
