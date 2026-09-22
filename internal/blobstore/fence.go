package blobstore

import (
	"context"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

const mutationFenceWeight int64 = 1 << 30

// MutationFencer waits for active mutations and prevents new ones until the
// returned release function is called. Reads remain available during a move.
type MutationFencer interface {
	BeginMutationFence(context.Context) (func(), error)
}

type fencedStore struct {
	Store
	mutations *semaphore.Weighted
}

func (s *fencedStore) Put(ctx context.Context, key string, data []byte) error {
	if err := s.mutations.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.mutations.Release(1)
	return s.Store.Put(ctx, key, data)
}

func (s *fencedStore) PutStream(ctx context.Context, key string, r io.Reader, contentType string) error {
	if err := s.mutations.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.mutations.Release(1)
	return s.Store.PutStream(ctx, key, r, contentType)
}

func (s *fencedStore) Delete(ctx context.Context, keys []string) (int, error) {
	if err := s.mutations.Acquire(ctx, 1); err != nil {
		return 0, err
	}
	defer s.mutations.Release(1)
	return s.Store.Delete(ctx, keys)
}

func (s *fencedStore) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if err := s.mutations.Acquire(ctx, 1); err != nil {
		return 0, err
	}
	defer s.mutations.Release(1)
	return s.Store.DeletePrefix(ctx, prefix)
}

// Matches only reads, so it passes through an active fence. Forwarding it keeps
// the image cache's immutable-object reuse check working on a fenced store.
func (s *fencedStore) Matches(ctx context.Context, key string, data []byte) (bool, error) {
	if matcher, ok := s.Store.(interface {
		Matches(context.Context, string, []byte) (bool, error)
	}); ok {
		return matcher.Matches(ctx, key, data)
	}
	return false, nil
}

func (s *fencedStore) BeginMutationFence(ctx context.Context) (func(), error) {
	if err := s.mutations.Acquire(ctx, mutationFenceWeight); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { s.mutations.Release(mutationFenceWeight) }) }, nil
}

type fencedDirectStore struct {
	*fencedStore
	direct DirectURLer
}

func (s *fencedDirectStore) DirectURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s.direct.DirectURL(ctx, key, ttl)
}

// WithMutationFence wraps a store once and preserves optional direct delivery.
func WithMutationFence(store Store) Store {
	if store == nil {
		return nil
	}
	if _, ok := store.(MutationFencer); ok {
		return store
	}
	base := &fencedStore{Store: store, mutations: semaphore.NewWeighted(mutationFenceWeight)}
	if direct, ok := store.(DirectURLer); ok {
		return &fencedDirectStore{fencedStore: base, direct: direct}
	}
	return base
}
