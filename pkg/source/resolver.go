package source

import (
	"context"
	"fmt"
)

// Resolver ties a Cache to per-kind fetchers and turns a resolved source plus
// trimming spec into a pinned snapshot and derived context.
//
// The distinction between "decision digest" and "materialized context" is
// deliberate:
//
//   - Snapshot.Digest is the whole-revision digest. If trimming is disabled,
//     build decisions and build inputs both use it, preserving the exact
//     pre-existing behavior.
//   - Context.Digest is the digest of only the selected subtree/files. It lets
//     an Image skip rebuilds when unrelated parts of a monorepo change, at the
//     cost of fetching content before the decision can be made.
//
// Callers that prefer the cheaper whole-revision decision can compare
// Snapshot.Digest and filter at build time; callers that want precision
// compare Context.Digest. The cache makes both strategies share one fetch.
type Resolver struct {
	Cache    *Cache
	Fetchers map[Kind]Fetcher
}

// NewResolver constructs a resolver with the provided fetchers.
func NewResolver(cache *Cache, fetchers map[Kind]Fetcher) *Resolver {
	if cache == nil {
		cache = NewCache()
	}
	return &Resolver{Cache: cache, Fetchers: fetchers}
}

// Fetcher returns the fetcher registered for a source kind.
func (r *Resolver) fetcher(kind Kind) (Fetcher, error) {
	fetcher, ok := r.Fetchers[kind]
	if !ok || fetcher == nil {
		return nil, fmt.Errorf("no source fetcher registered for kind %q", kind)
	}
	return fetcher, nil
}

// Snapshot returns the full, revision-pinned snapshot for a source, fetching
// it at most once per (identity, revision) across all Images sharing this
// resolver's cache.
func (r *Resolver) Snapshot(ctx context.Context, pinned Pinned) (*Snapshot, error) {
	fetcher, err := r.fetcher(pinned.Identity.Kind)
	if err != nil {
		return nil, err
	}
	return r.Cache.GetSnapshot(ctx, pinned, fetcher)
}

// Context returns the trimmed context for a pinned source.
func (r *Resolver) Context(ctx context.Context, pinned Pinned, spec ContextSpec) (*Context, error) {
	fetcher, err := r.fetcher(pinned.Identity.Kind)
	if err != nil {
		return nil, err
	}
	return r.Cache.GetContext(ctx, pinned, spec, fetcher)
}
