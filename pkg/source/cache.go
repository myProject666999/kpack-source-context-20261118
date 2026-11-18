package source

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"
)

// Snapshot is the complete, immutable set of files for one pinned revision.
// Entries are relative to the source root and use "/" separators. Files holds
// the bytes for entries that can be materialized locally; it may be nil when
// the snapshot exists only to compute a digest.
type Snapshot struct {
	Identity Identity
	Revision Revision
	Entries  []Entry
	Files    map[string][]byte
}

// Validate checks the snapshot's internal consistency.
func (s *Snapshot) Validate() error {
	if s == nil {
		return fmt.Errorf("snapshot must not be nil")
	}
	if _, err := NewPinned(s.Identity, s.Revision); err != nil {
		return err
	}
	for _, entry := range s.Entries {
		if entry.Path == "" {
			return contextError(ErrEscapesRoot, "snapshot contains an empty path")
		}
		if hasUpLevelComponent(entry.Path) || strings.HasPrefix(entry.Path, "/") {
			return contextError(ErrEscapesRoot, "snapshot entry %q is not inside the source root", entry.Path)
		}
	}
	return nil
}

// Context derives a trimmed view of the snapshot. Symlinks that resolve
// outside the context root are rejected rather than silently followed.
func (s *Snapshot) Context(spec ContextSpec) (*Context, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	entries, err := selectEntries(s.Entries, spec)
	if err != nil {
		return nil, err
	}

	normalized, err := spec.Normalize()
	if err != nil {
		return nil, err
	}

	if err := validateSymlinks(entries, s.Files); err != nil {
		return nil, err
	}

	result := make([]Entry, 0, len(entries))
	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Entry)
		if content, ok := s.Files[entry.SourcePath]; ok {
			files[entry.Path] = content
		}
	}

	return &Context{
		Digest:   DigestEntries(result),
		Entries:  result,
		Files:    files,
		Spec:     normalized,
		Snapshot: s,
	}, nil
}

// FullContext is the untrimmed view used to preserve legacy build inputs.
func (s *Snapshot) FullContext() *Context {
	entries := append([]Entry(nil), s.Entries...)
	files := make(map[string][]byte, len(s.Files))
	for path, content := range s.Files {
		files[path] = content
	}
	return &Context{
		Digest:   DigestEntries(entries),
		Entries:  entries,
		Files:    files,
		Spec:     ContextSpec{},
		Snapshot: s,
	}
}

// validateSymlinks rejects symlinks whose target resolves outside the source
// root. A symlink to another path inside the root is allowed because npm-style
// workspace layouts depend on them, but it must not be a path escape. Targets
// are interpreted relative to the symlink's directory and normalized lexically,
// matching how git stores link targets.
func validateSymlinks(entries []selectedEntry, files map[string][]byte) error {
	for _, entry := range entries {
		if entry.Type != EntrySymlink {
			continue
		}
		target, ok := files[entry.SourcePath]
		if !ok {
			continue
		}
		// Validate against the source-root-relative path: the rule is that a
		// selected symlink must not escape the source root, regardless of how
		// the context trimmed the surrounding tree.
		if err := checkSymlinkTarget(entry.SourcePath, string(target)); err != nil {
			return err
		}
	}
	return nil
}

func checkSymlinkTarget(path, target string) error {
	if target == "" {
		return contextError(ErrSymlinkEscape, "symlink %q has an empty target", path)
	}
	// Absolute targets always point outside a relative source root.
	if strings.HasPrefix(target, "/") || windowsAbsolutePath.MatchString(target) || strings.HasPrefix(target, `\\`) {
		return contextError(ErrSymlinkEscape, "symlink %q points outside the source root with absolute target %q", path, target)
	}

	dir := ""
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		dir = path[:idx]
	}

	components := []string{}
	if dir != "" {
		components = strings.Split(dir, "/")
	}
	for _, component := range strings.Split(strings.ReplaceAll(target, `\`, "/"), "/") {
		switch component {
		case "", ".":
			continue
		case "..":
			if len(components) == 0 {
				// Popping above the source root escapes it.
				return contextError(ErrSymlinkEscape, "symlink %q escapes the source root with target %q", path, target)
			}
			components = components[:len(components)-1]
		default:
			components = append(components, component)
		}
	}
	return nil
}

type Context struct {
	// Digest is the build-input digest of the selected entries. It is stable
	// for identical Source, and changes only when selected content or its
	// path/mode changes.
	Digest string
	// Entries are relative to the context root.
	Entries []Entry
	// Files maps context-relative paths to content for materializable entries.
	Files map[string][]byte
	// Spec is the normalized trimming that produced this context.
	Spec ContextSpec
	// Snapshot is the full (untrimmed) snapshot.
	Snapshot *Snapshot
}

// Cache shares full source snapshots and derived contexts between Images.
//
// Full snapshots are keyed by (identity, revision); derived contexts extend
// that key with the canonical trimming parameters. Concurrent requests for the
// same key share a single fetch. A snapshot is stored only after a fully
// successful fetch; a failed or cancelled fetch leaves nothing readable.
type Cache struct {
	mu        sync.Mutex
	snapshots map[string]*cacheEntry[*Snapshot]
	contexts  map[string]*cacheEntry[*Context]
	fetching  singleflight.Group
	building  singleflight.Group
}

type cacheEntry[T any] struct {
	value T
	// refs counts live leases. An entry with refs > 0 is pinned against
	// eviction even when it is not the most recently used.
	refs int
}

// NewCache returns an empty, ready-to-use cache.
func NewCache() *Cache {
	return &Cache{
		snapshots: map[string]*cacheEntry[*Snapshot]{},
		contexts:  map[string]*cacheEntry[*Context]{},
	}
}

// Fetcher obtains the complete snapshot for a pinned source. Implementations
// must be safe to call concurrently for different keys and must return only
// once the content is fully available.
type Fetcher interface {
	Fetch(ctx context.Context, pinned Pinned) (*Snapshot, error)
}

// FetcherFunc adapts a function to Fetcher.
type FetcherFunc func(ctx context.Context, pinned Pinned) (*Snapshot, error)

// Fetch implements Fetcher.
func (f FetcherFunc) Fetch(ctx context.Context, pinned Pinned) (*Snapshot, error) {
	return f(ctx, pinned)
}

// DiscoveringFetcher obtains a snapshot for a source whose immutable revision
// is only known after reading the content (for example a blob's archive digest
// or a source image's manifest digest). It fills in Snapshot.Revision.
type DiscoveringFetcher interface {
	Discover(ctx context.Context, identity Identity) (*Snapshot, error)
}

// GetDiscoveredSnapshot fetches and caches a snapshot for a source whose
// revision is not known ahead of time. The snapshot is stored under the
// revision the fetcher discovered, so a later known-revision fetch of the same
// content shares the cached entry.
func (c *Cache) GetDiscoveredSnapshot(ctx context.Context, identity Identity, fetcher DiscoveringFetcher) (*Snapshot, error) {
	if _, err := NewIdentity(identity.Kind, identity.Location, identity.Variant); err != nil {
		return nil, err
	}
	discoveryKey := "discover:" + identity.Key()

	result, err, _ := c.fetching.Do(discoveryKey, func() (interface{}, error) {
		snapshot, err := fetcher.Discover(ctx, identity)
		if err != nil {
			return nil, err
		}
		if err := snapshot.Validate(); err != nil {
			return nil, err
		}
		snapshot.Identity = identity

		key := Pinned{Identity: identity, Revision: snapshot.Revision}.CacheKey()
		c.mu.Lock()
		c.snapshots[key] = &cacheEntry[*Snapshot]{value: snapshot}
		c.mu.Unlock()
		return snapshot, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*Snapshot), nil
}

// GetSnapshot returns the shared snapshot for a pinned source, fetching it at
// most once across concurrent callers. On error nothing is cached.
func (c *Cache) GetSnapshot(ctx context.Context, pinned Pinned, fetcher Fetcher) (*Snapshot, error) {
	if _, err := NewPinned(pinned.Identity, pinned.Revision); err != nil {
		return nil, err
	}
	key := pinned.CacheKey()

	c.mu.Lock()
	if entry, ok := c.snapshots[key]; ok && entry.value != nil {
		c.mu.Unlock()
		return entry.value, nil
	}
	c.mu.Unlock()

	result, err, _ := c.fetching.Do(key, func() (interface{}, error) {
		// Re-check under the singleflight leader; a prior leader may have
		// populated the cache while this caller waited.
		c.mu.Lock()
		if entry, ok := c.snapshots[key]; ok && entry.value != nil {
			c.mu.Unlock()
			return entry.value, nil
		}
		c.mu.Unlock()

		snapshot, err := fetcher.Fetch(ctx, pinned)
		if err != nil {
			// Never publish a partial snapshot.
			return nil, err
		}
		if err := snapshot.Validate(); err != nil {
			return nil, err
		}
		// Normalize the identity/revision on the stored copy so callers that
		// share a key cannot observe a mismatched owner.
		snapshot.Identity = pinned.Identity
		snapshot.Revision = pinned.Revision

		c.mu.Lock()
		c.snapshots[key] = &cacheEntry[*Snapshot]{value: snapshot}
		c.mu.Unlock()
		return snapshot, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*Snapshot), nil
}

// GetContext returns the derived context for a pinned source and trimming
// spec, reusing both the full snapshot and any previously derived context.
func (c *Cache) GetContext(ctx context.Context, pinned Pinned, spec ContextSpec, fetcher Fetcher) (*Context, error) {
	normalized, err := spec.Normalize()
	if err != nil {
		return nil, err
	}

	if normalized.IsTrivial() {
		// Preserve the historical, untrimmed build input exactly.
		snapshot, err := c.GetSnapshot(ctx, pinned, fetcher)
		if err != nil {
			return nil, err
		}
		return snapshot.FullContext(), nil
	}

	contextKey, err := pinned.ContextKey(normalized)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if entry, ok := c.contexts[contextKey]; ok && entry.value != nil {
		c.mu.Unlock()
		return entry.value, nil
	}
	c.mu.Unlock()

	result, err, _ := c.building.Do(contextKey, func() (interface{}, error) {
		c.mu.Lock()
		if entry, ok := c.contexts[contextKey]; ok && entry.value != nil {
			c.mu.Unlock()
			return entry.value, nil
		}
		c.mu.Unlock()

		snapshot, err := c.GetSnapshot(ctx, pinned, fetcher)
		if err != nil {
			return nil, err
		}

		derived, err := snapshot.Context(normalized)
		if err != nil {
			return nil, err
		}

		c.mu.Lock()
		c.contexts[contextKey] = &cacheEntry[*Context]{value: derived}
		c.mu.Unlock()
		return derived, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*Context), nil
}

// Lease pins an existing cache key against eviction until Release is called.
// Leasing an absent key is a no-op so callers can lease before or after a
// fetch. The returned function is safe to call more than once.
func (c *Cache) Lease(snapshotKey, contextKey string) (release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var released bool
	if entry, ok := c.snapshots[snapshotKey]; ok {
		entry.refs++
	}
	if entry, ok := c.contexts[contextKey]; ok {
		entry.refs++
	}

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if released {
			return
		}
		released = true
		if entry, ok := c.snapshots[snapshotKey]; ok && entry.refs > 0 {
			entry.refs--
		}
		if entry, ok := c.contexts[contextKey]; ok && entry.refs > 0 {
			entry.refs--
		}
	}
}

// EvictContext removes a derived context if it is not in use. It returns true
// when the entry was removed.
func (c *Cache) EvictContext(contextKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.contexts[contextKey]
	if !ok || entry.refs > 0 {
		return false
	}
	delete(c.contexts, contextKey)
	return true
}

// EvictSnapshot removes a full snapshot and every derived context built from
// it, skipping any entry currently referenced. It returns true when the
// snapshot itself was removed.
func (c *Cache) EvictSnapshot(snapshotKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.snapshots[snapshotKey]
	if !ok || entry.refs > 0 {
		return false
	}
	delete(c.snapshots, snapshotKey)
	for key, contextEntry := range c.contexts {
		if contextEntry.refs > 0 {
			continue
		}
		if len(key) > len(snapshotKey) && key[:len(snapshotKey)] == snapshotKey {
			delete(c.contexts, key)
		}
	}
	return true
}

// Len reports the number of cached snapshots and contexts. It exists for tests
// and diagnostics.
func (c *Cache) Len() (snapshots, contexts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.snapshots), len(c.contexts)
}
