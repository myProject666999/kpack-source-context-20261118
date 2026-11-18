package blob_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sclevine/spec"
	"github.com/stretchr/testify/require"

	"github.com/pivotal/kpack/pkg/blob"
	"github.com/pivotal/kpack/pkg/source"
)

func TestBlobSnapshotFetcher(t *testing.T) {
	spec.Run(t, "Blob snapshot fetcher", testBlobSnapshotFetcher)
}

func testBlobSnapshotFetcher(t *testing.T, when spec.G, it spec.S) {
	when("pinning blobs", func() {
		it("fetches once, discovers the digest, and shares content between subpaths", func() {
			archive := tarGzArchive(t, map[string]string{
				"apps/a/main.go": "package main // a",
				"apps/b/main.go": "package main // b",
			})

			var requests int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt64(&requests, 1)
				w.Write(archive)
			}))
			defer server.Close()

			cache := source.NewCache()
			fetcher := &blob.SnapshotFetcher{Keychain: nil}
			identity, err := source.NewIdentity(source.KindBlob, server.URL+"/src.tar.gz", "")
			require.NoError(t, err)

			snapshot, err := cache.GetDiscoveredSnapshot(context.Background(), identity, fetcher)
			require.NoError(t, err)
			require.Equal(t, source.DigestFromBytes(archive), snapshot.Revision.Value)

			a, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)
			b, err := snapshot.Context(source.ContextSpec{SubPath: "apps/b"})
			require.NoError(t, err)
			require.NotEqual(t, a.Digest, b.Digest)
			require.Equal(t, int64(1), atomic.LoadInt64(&requests))
		})

		it("rejects content whose digest does not match the pinned revision", func() {
			archive := zipArchive(t, map[string]string{"main.go": "package main"})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(archive)
			}))
			defer server.Close()

			identity, err := source.NewIdentity(source.KindBlob, server.URL+"/src.zip", "")
			require.NoError(t, err)
			wrongRevision, err := source.DigestRevision(source.DigestFromBytes([]byte("something else")))
			require.NoError(t, err)
			pinned, err := source.NewPinned(identity, wrongRevision)
			require.NoError(t, err)

			fetcher := &blob.SnapshotFetcher{}
			_, err = fetcher.Fetch(context.Background(), pinned)
			require.Error(t, err)
			require.Contains(t, err.Error(), "does not match pinned revision")
		})

		it("supports zip, tar and tar.gz archives", func() {
			for name, archive := range map[string][]byte{
				"src.zip":    zipArchive(t, map[string]string{"app/main.go": "package main"}),
				"src.tar":    tarArchive(map[string]string{"app/main.go": "package main"}),
				"src.tar.gz": tarGzArchive(t, map[string]string{"app/main.go": "package main"}),
			} {
				archive := archive
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write(archive)
				}))

				identity, err := source.NewIdentity(source.KindBlob, server.URL+"/"+name, "")
				require.NoError(t, err)
				snapshot, err := (&blob.SnapshotFetcher{}).Discover(context.Background(), identity)
				server.Close()
				require.NoError(t, err, name)

				context, err := snapshot.Context(source.ContextSpec{SubPath: "app"})
				require.NoError(t, err, name)
				require.Equal(t, []byte("package main"), context.Files["main.go"], name)
			}
		})
	})
}

func tarArchive(files map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		tw.Write([]byte(content))
	}
	tw.Close()
	return buf.Bytes()
}

func tarGzArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func zipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}
