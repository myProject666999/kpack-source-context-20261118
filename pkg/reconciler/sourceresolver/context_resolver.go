package sourceresolver

import (
	"context"
	"errors"
	"fmt"

	corev1alpha1 "github.com/pivotal/kpack/pkg/apis/core/v1alpha1"
	"github.com/pivotal/kpack/pkg/source"
)

// SourceContextResolver pins a resolved source to an immutable revision and,
// when trimming is requested, computes the content digest of the resulting
// build context. It shares full source snapshots between Images through the
// cache it was constructed with.
type SourceContextResolver interface {
	// ResolveContext returns the resolved source config to record in status,
	// with its revision pinned and (when trimming) its context digest set.
	// When trimming is not requested the resolved config is returned unchanged
	// so legacy Images are byte-for-byte identical. previous is the last
	// recorded status and lets a discovered digest be reused instead of
	// re-read on every reconcile.
	ResolveContext(ctx context.Context, namespace, serviceAccount string, config corev1alpha1.SourceConfig, resolved, previous corev1alpha1.ResolvedSourceConfig) (corev1alpha1.ResolvedSourceConfig, error)
}

// FetcherFactory builds a source.Fetcher for a specific SourceResolver's
// namespace and service account. Credentials are namespaced, so the factory is
// invoked once per reconcile rather than being shared globally.
type FetcherFactory func(ctx context.Context, kind source.Kind, namespace, serviceAccount string) (source.Fetcher, error)

// NewContextResolver returns a SourceContextResolver backed by cache and the
// supplied fetcher factory.
func NewContextResolver(cache *source.Cache, factory FetcherFactory) *contextResolver {
	if cache == nil {
		cache = source.NewCache()
	}
	return &contextResolver{cache: cache, factory: factory}
}

type contextResolver struct {
	cache   *source.Cache
	factory FetcherFactory
}

// ResolveContext implements SourceContextResolver.
func (r *contextResolver) ResolveContext(ctx context.Context, namespace, serviceAccount string, config corev1alpha1.SourceConfig, resolved, previous corev1alpha1.ResolvedSourceConfig) (corev1alpha1.ResolvedSourceConfig, error) {
	spec, err := (source.ContextSpec{SubPath: config.SubPath, Excludes: config.Excludes}).Normalize()
	if err != nil {
		return resolved, err
	}
	if spec.IsTrivial() {
		// Compatibility guarantee: no subpath and no excludes means no
		// pinning, no digest and no extra fetch.
		return resolved, nil
	}

	if resolved.Git != nil && resolved.Git.IsUnknown() {
		// The source could not be resolved (for example an authentication
		// failure). Leave the unknown status intact so the reconcile records a
		// clear "not reconcilable" status rather than a pinning error.
		return resolved, nil
	}

	if resolved.Git != nil && resolved.Git.Tree != "" {
		// The shallow-clone experiment already computes a per-subpath tree and
		// retains the previous revision when it is unchanged. Do not layer a
		// second, differently-timed decision mechanism on top of it.
		return resolved, nil
	}

	identity, kind, err := identityFor(resolved)
	if err != nil {
		return resolved, err
	}

	fetcher, err := r.factory(ctx, kind, namespace, serviceAccount)
	if err != nil {
		return resolved, err
	}

	pinned, err := r.pinOrDiscover(ctx, identity, kind, resolved, previous, fetcher)
	if err != nil {
		return resolved, err
	}

	derived, err := r.cache.GetContext(ctx, pinned, spec, fetcher)
	if err != nil {
		return resolved, err
	}

	applyPinned(&resolved, pinned.Revision)
	applyContext(&resolved, spec, derived.Digest)
	return resolved, nil
}

// pinOrDiscover returns the pinned source. Git revisions are already fixed by
// the resolver (a commit hash). Blobs and registry images are pinned only when
// the resolver discovered a digest; otherwise discovery reads the content once
// and records the digest so later builds reuse the same revision.
func (r *contextResolver) pinOrDiscover(ctx context.Context, identity source.Identity, kind source.Kind, resolved, previous corev1alpha1.ResolvedSourceConfig, fetcher source.Fetcher) (source.Pinned, error) {
	// A digest recorded in the previous status came from the same location
	// (the spec has not changed) and is reused so an unchanged source is not
	// re-downloaded on every reconcile.
	existing := revisionFrom(resolved)
	if existing == "" {
		existing = previousRevisionFrom(previous, identity)
	}
	if existing != "" {
		revision, err := source.NewRevision(kind, existing)
		if err != nil {
			return source.Pinned{}, err
		}
		return source.NewPinned(identity, revision)
	}

	discovering, ok := fetcher.(source.DiscoveringFetcher)
	if !ok {
		return source.Pinned{}, fmt.Errorf("source kind %q requires discovery but its fetcher does not support it", kind)
	}
	snapshot, err := r.cache.GetDiscoveredSnapshot(ctx, identity, discovering)
	if err != nil {
		return source.Pinned{}, err
	}
	return source.NewPinned(identity, snapshot.Revision)
}

func revisionFrom(resolved corev1alpha1.ResolvedSourceConfig) string {
	switch {
	case resolved.Git != nil:
		return resolved.Git.Revision
	case resolved.Blob != nil:
		return resolved.Blob.Digest
	case resolved.Registry != nil:
		return resolved.Registry.Digest
	default:
		return ""
	}
}

// previousRevisionFrom reuses a previously discovered digest, but only for the
// same location. A changed URL or image means the old digest is meaningless.
func previousRevisionFrom(previous corev1alpha1.ResolvedSourceConfig, identity source.Identity) string {
	switch {
	case previous.Blob != nil && identity.Kind == source.KindBlob && previous.Blob.URL == identity.Location:
		return previous.Blob.Digest
	case previous.Registry != nil && identity.Kind == source.KindRegistry && previous.Registry.Image == identity.Location:
		return previous.Registry.Digest
	default:
		return ""
	}
}

func identityFor(resolved corev1alpha1.ResolvedSourceConfig) (source.Identity, source.Kind, error) {
	switch {
	case resolved.Git != nil:
		identity, err := source.NewIdentity(source.KindGit, resolved.Git.URL, "")
		return identity, source.KindGit, err
	case resolved.Blob != nil:
		identity, err := source.NewIdentity(source.KindBlob, resolved.Blob.URL, blobVariant(resolved.Blob.StripComponents))
		return identity, source.KindBlob, err
	case resolved.Registry != nil:
		identity, err := source.NewIdentity(source.KindRegistry, resolved.Registry.Image, "")
		return identity, source.KindRegistry, err
	default:
		return source.Identity{}, "", errors.New("resolved source is empty")
	}
}

func applyPinned(resolved *corev1alpha1.ResolvedSourceConfig, revision source.Revision) {
	switch {
	case resolved.Git != nil:
		resolved.Git.Revision = revision.Value
	case resolved.Blob != nil:
		resolved.Blob.Digest = revision.Value
	case resolved.Registry != nil:
		resolved.Registry.Digest = revision.Value
	}
}

func blobVariant(stripComponents int64) string {
	if stripComponents == 0 {
		return ""
	}
	return fmt.Sprintf("strip-components=%d", stripComponents)
}

func applyContext(resolved *corev1alpha1.ResolvedSourceConfig, spec source.ContextSpec, digest string) {
	switch {
	case resolved.Git != nil:
		resolved.Git.ContextDigest = digest
		resolved.Git.Excludes = spec.Excludes
	case resolved.Blob != nil:
		resolved.Blob.ContextDigest = digest
		resolved.Blob.Excludes = spec.Excludes
	case resolved.Registry != nil:
		resolved.Registry.ContextDigest = digest
		resolved.Registry.Excludes = spec.Excludes
	}
}

var _ SourceContextResolver = (*contextResolver)(nil)
