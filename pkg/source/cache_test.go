package source_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sclevine/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pivotal/kpack/pkg/source"
)

func TestCache(t *testing.T) {
	spec.Run(t, "Source cache", testCache)
}

func testCache(t *testing.T, when spec.G, it spec.S) {
	pinned := func(revision string) source.Pinned {
		identity, err := source.NewIdentity(source.KindGit, "https://example/repo", "")
		require.NoError(t, err)
		rev, err := source.GitRevision(revision)
		require.NoError(t, err)
		p, err := source.NewPinned(identity, rev)
		require.NoError(t, err)
		return p
	}

	snapshotFor := func(p source.Pinned, files map[string]string) *source.Snapshot {
		snapshot := &source.Snapshot{Identity: p.Identity, Revision: p.Revision, Files: map[string][]byte{}}
		for path, content := range files {
			snapshot.Entries = append(snapshot.Entries, source.Entry{Path: path, Type: source.EntryFile, Mode: "644", Hash: source.DigestFromBytes([]byte(content))})
			snapshot.Files[path] = []byte(content)
		}
		return snapshot
	}

	const rev = "0123456789abcdef0123456789abcdef01234567"
	const rev2 = "1123456789abcdef0123456789abcdef01234567"

	when("fetching snapshots", func() {
		it("fetches a revision only once across concurrent callers", func() {
			cache := source.NewCache()
			var calls int64
			release := make(chan struct{})
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				atomic.AddInt64(&calls, 1)
				<-release
				return snapshotFor(p, map[string]string{"main.go": "package main"}), nil
			})

			const callers = 20
			var wg sync.WaitGroup
			wg.Add(callers)
			results := make([]*source.Snapshot, callers)
			errs := make([]error, callers)
			for i := 0; i < callers; i++ {
				go func(i int) {
					defer wg.Done()
					results[i], errs[i] = cache.GetSnapshot(context.Background(), pinned(rev), fetcher)
				}(i)
			}

			// Let the callers pile up behind the single in-flight fetch.
			time.Sleep(50 * time.Millisecond)
			close(release)
			wg.Wait()

			assert.Equal(t, int64(1), atomic.LoadInt64(&calls))
			for i := 0; i < callers; i++ {
				require.NoError(t, errs[i])
				assert.Same(t, results[0], results[i])
			}
		})

		it("does not cache an incomplete fetch after a failure", func() {
			cache := source.NewCache()
			var attempt int64
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				if atomic.AddInt64(&attempt, 1) == 1 {
					return nil, errors.New("network died mid-transfer")
				}
				return snapshotFor(p, map[string]string{"main.go": "complete"}), nil
			})

			_, err := cache.GetSnapshot(context.Background(), pinned(rev), fetcher)
			require.Error(t, err)

			snapshots, _ := cache.Len()
			assert.Equal(t, 0, snapshots, "a failed fetch must not be readable as a complete cache entry")

			snapshot, err := cache.GetSnapshot(context.Background(), pinned(rev), fetcher)
			require.NoError(t, err)
			assert.Equal(t, []byte("complete"), snapshot.Files["main.go"])
		})

		it("never publishes a snapshot that fails validation", func() {
			cache := source.NewCache()
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				return &source.Snapshot{Entries: []source.Entry{{Path: "../escape", Type: source.EntryFile}}}, nil
			})

			_, err := cache.GetSnapshot(context.Background(), pinned(rev), fetcher)
			require.Error(t, err)
			snapshots, _ := cache.Len()
			assert.Equal(t, 0, snapshots)
		})
	})

	when("deriving contexts", func() {
		it("shares the underlying snapshot across different contexts", func() {
			cache := source.NewCache()
			var calls int64
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				atomic.AddInt64(&calls, 1)
				return snapshotFor(p, map[string]string{"apps/a/main.go": "a", "apps/b/main.go": "b"}), nil
			})

			a, err := cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{SubPath: "apps/a"}, fetcher)
			require.NoError(t, err)
			b, err := cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{SubPath: "apps/b"}, fetcher)
			require.NoError(t, err)

			assert.Equal(t, int64(1), atomic.LoadInt64(&calls), "the full source should be fetched once")
			assert.NotEqual(t, a.Digest, b.Digest)
		})

		it("keys derived contexts by trimming parameters", func() {
			cache := source.NewCache()
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				return snapshotFor(p, map[string]string{
					"apps/a/main.go":    "a",
					"apps/a/vendor/lib": "vendored",
					"apps/a/debug.log":  "log",
				}), nil
			})

			withVendor, err := cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{SubPath: "apps/a"}, fetcher)
			require.NoError(t, err)
			withoutVendor, err := cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{SubPath: "apps/a", Excludes: []string{"vendor/"}}, fetcher)
			require.NoError(t, err)

			assert.NotEqual(t, withVendor.Digest, withoutVendor.Digest)
		})

		it("reuses a derived context for repeated equivalent requests", func() {
			cache := source.NewCache()
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				return snapshotFor(p, map[string]string{"apps/a/main.go": "a"}), nil
			})

			a, err := cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{SubPath: "/apps/a/"}, fetcher)
			require.NoError(t, err)
			b, err := cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{SubPath: "apps/a"}, fetcher)
			require.NoError(t, err)

			assert.Same(t, a, b)
			snapshots, contexts := cache.Len()
			assert.Equal(t, 1, snapshots)
			assert.Equal(t, 1, contexts)
		})

		it("treats a trivial spec as the untrimmed context", func() {
			cache := source.NewCache()
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				return snapshotFor(p, map[string]string{"main.go": "a", "other/x": "b"}), nil
			})

			full, err := cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{}, fetcher)
			require.NoError(t, err)
			assert.Len(t, full.Entries, 2)
		})
	})

	when("leasing and eviction", func() {
		it("refuses to evict an entry that is in use", func() {
			cache := source.NewCache()
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				return snapshotFor(p, map[string]string{"main.go": "a"}), nil
			})

			_, err := cache.GetSnapshot(context.Background(), pinned(rev), fetcher)
			require.NoError(t, err)

			release := cache.Lease(pinned(rev).CacheKey(), "")
			assert.False(t, cache.EvictSnapshot(pinned(rev).CacheKey()))

			release()
			assert.True(t, cache.EvictSnapshot(pinned(rev).CacheKey()))
		})

		it("evicting a snapshot also evicts its idle derived contexts", func() {
			cache := source.NewCache()
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				return snapshotFor(p, map[string]string{"apps/a/main.go": "a"}), nil
			})

			_, err := cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{SubPath: "apps/a"}, fetcher)
			require.NoError(t, err)
			_, contexts := cache.Len()
			require.Equal(t, 1, contexts)

			assert.True(t, cache.EvictSnapshot(pinned(rev).CacheKey()))
			_, contexts = cache.Len()
			assert.Equal(t, 0, contexts)
		})

		it("re-fetches after eviction", func() {
			cache := source.NewCache()
			var calls int64
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				atomic.AddInt64(&calls, 1)
				return snapshotFor(p, map[string]string{"main.go": "a"}), nil
			})

			_, err := cache.GetSnapshot(context.Background(), pinned(rev), fetcher)
			require.NoError(t, err)
			require.True(t, cache.EvictSnapshot(pinned(rev).CacheKey()))
			_, err = cache.GetSnapshot(context.Background(), pinned(rev), fetcher)
			require.NoError(t, err)

			assert.Equal(t, int64(2), atomic.LoadInt64(&calls))
		})
	})

	when("discovering content-addressed sources", func() {
		it("stores the discovered snapshot under its revision so a later known-revision fetch reuses it", func() {
			cache := source.NewCache()
			var discovers, fetches int64
			identity, err := source.NewIdentity(source.KindBlob, "https://example/src.tar", "")
			require.NoError(t, err)

			discovering := discoveringFetcher(func(ctx context.Context, id source.Identity) (*source.Snapshot, error) {
				atomic.AddInt64(&discovers, 1)
				revision, err := source.DigestRevision(source.DigestFromBytes([]byte("content")))
				require.NoError(t, err)
				return &source.Snapshot{
					Identity: id,
					Revision: revision,
					Entries:  []source.Entry{{Path: "main.go", Type: source.EntryFile, Mode: "644", Hash: "h"}},
					Files:    map[string][]byte{"main.go": []byte("content")},
				}, nil
			})
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				atomic.AddInt64(&fetches, 1)
				return snapshotFor(p, map[string]string{"main.go": "content"}), nil
			})

			snapshot, err := cache.GetDiscoveredSnapshot(context.Background(), identity, discovering)
			require.NoError(t, err)

			pinned, err := source.NewPinned(identity, snapshot.Revision)
			require.NoError(t, err)
			_, err = cache.GetSnapshot(context.Background(), pinned, fetcher)
			require.NoError(t, err)

			assert.Equal(t, int64(1), atomic.LoadInt64(&discovers))
			assert.Equal(t, int64(0), atomic.LoadInt64(&fetches), "the known-revision fetch should reuse the discovered snapshot")
		})
	})

	when("concurrent writes", func() {
		it("is race-free while many goroutines populate the same keys", func() {
			cache := source.NewCache()
			fetcher := source.FetcherFunc(func(ctx context.Context, p source.Pinned) (*source.Snapshot, error) {
				return snapshotFor(p, map[string]string{"apps/a/main.go": "a"}), nil
			})

			var wg sync.WaitGroup
			for i := 0; i < 50; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = cache.GetContext(context.Background(), pinned(rev), source.ContextSpec{SubPath: "apps/a"}, fetcher)
					_, _ = cache.GetSnapshot(context.Background(), pinned(rev), fetcher)
					release := cache.Lease(pinned(rev).CacheKey(), "")
					release()
				}()
			}
			wg.Wait()

			snapshots, _ := cache.Len()
			assert.Equal(t, 1, snapshots)
		})
	})
}

type discoveringFetcher func(ctx context.Context, identity source.Identity) (*source.Snapshot, error)

func (f discoveringFetcher) Discover(ctx context.Context, identity source.Identity) (*source.Snapshot, error) {
	return f(ctx, identity)
}
