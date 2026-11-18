package registry_test

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/sclevine/spec"
	"github.com/stretchr/testify/require"

	"github.com/pivotal/kpack/pkg/registry"
	"github.com/pivotal/kpack/pkg/registry/registryfakes"
	"github.com/pivotal/kpack/pkg/source"
)

func TestRegistrySnapshotFetcher(t *testing.T) {
	spec.Run(t, "Registry snapshot fetcher", testRegistrySnapshotFetcher)
}

func testRegistrySnapshotFetcher(t *testing.T, when spec.G, it spec.S) {
	when("pinning a source image", func() {
		it("discovers the manifest digest and can derive subpath contexts", func() {
			keychain := &registryfakes.FakeKeychain{}
			client := registryfakes.NewFakeClient()

			files := map[string]string{
				"apps/a/main.go": "package main // a",
				"apps/b/main.go": "package main // b",
			}
			img := sourceImage(t, files)
			repoName := "registry.example/monorepo"
			client.AddImage(repoName, img, keychain)

			expectedDigest, err := img.Digest()
			require.NoError(t, err)

			fetcher := &registrySnapshotFetcherForTest{client: client, keychain: keychain}
			identity, err := source.NewIdentity(source.KindRegistry, repoName, "")
			require.NoError(t, err)

			cache := source.NewCache()
			snapshot, err := cache.GetDiscoveredSnapshot(context.Background(), identity, fetcher)
			require.NoError(t, err)
			require.Equal(t, expectedDigest.String(), snapshot.Revision.Value)

			a, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)
			aAgain, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)
			b, err := snapshot.Context(source.ContextSpec{SubPath: "apps/b"})
			require.NoError(t, err)

			require.Equal(t, a.Digest, aAgain.Digest)
			require.NotEqual(t, a.Digest, b.Digest)
		})

		it("aborts when the image no longer matches the pinned digest", func() {
			keychain := &registryfakes.FakeKeychain{}
			client := registryfakes.NewFakeClient()
			img := sourceImage(t, map[string]string{"main.go": "package main"})
			client.AddImage("registry.example/moved", img, keychain)

			identity, err := source.NewIdentity(source.KindRegistry, "registry.example/moved", "")
			require.NoError(t, err)
			wrong, err := source.DigestRevision(source.DigestFromBytes([]byte("stale")))
			require.NoError(t, err)
			pinned, err := source.NewPinned(identity, wrong)
			require.NoError(t, err)

			fetcher := &registry.SnapshotFetcher{Keychain: keychain, Client: client}
			_, err = fetcher.Fetch(context.Background(), pinned)
			require.Error(t, err)
			require.Contains(t, err.Error(), "not the pinned digest")
		})
	})
}

// registrySnapshotFetcherForTest adapts the fake client into a snapshot
// fetcher without exposing test-only hooks in production code beyond
// Client.
type registrySnapshotFetcherForTest struct {
	client   *registryfakes.FakeClient
	keychain authn.Keychain
}

func (f *registrySnapshotFetcherForTest) Discover(ctx context.Context, identity source.Identity) (*source.Snapshot, error) {
	return (&registry.SnapshotFetcher{Keychain: f.keychain, Client: f.client}).Discover(ctx, identity)
}

func (f *registrySnapshotFetcherForTest) Fetch(ctx context.Context, pinned source.Pinned) (*source.Snapshot, error) {
	return (&registry.SnapshotFetcher{Keychain: f.keychain, Client: f.client}).Fetch(ctx, pinned)
}

func sourceImage(t *testing.T, files map[string]string) v1.Image {
	t.Helper()
	img, err := random.Image(0, 0)
	require.NoError(t, err)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())

	layer, err := tarball.LayerFromReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)

	img, err = mutate.AppendLayers(img, layer)
	require.NoError(t, err)
	return img
}
