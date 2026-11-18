package source_test

import (
	"testing"

	"github.com/sclevine/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pivotal/kpack/pkg/source"
)

func TestIdentity(t *testing.T) {
	spec.Run(t, "Source identity", testIdentity)
}

func testIdentity(t *testing.T, when spec.G, it spec.S) {
	when("constructing identities", func() {
		it("rejects unknown kinds and empty locations", func() {
			_, err := source.NewIdentity("ftp", "x", "")
			require.Error(t, err)
			_, err = source.NewIdentity(source.KindGit, "   ", "")
			require.Error(t, err)
		})

		it("partitions the cache key by kind, location and variant", func() {
			a, err := source.NewIdentity(source.KindBlob, "https://example/x.tar", "strip-components=1")
			require.NoError(t, err)
			b, err := source.NewIdentity(source.KindBlob, "https://example/x.tar", "strip-components=2")
			require.NoError(t, err)
			c, err := source.NewIdentity(source.KindGit, "https://example/x.tar", "strip-components=1")
			require.NoError(t, err)

			assert.NotEqual(t, a.Key(), b.Key())
			assert.NotEqual(t, a.Key(), c.Key())
		})
	})

	when("constructing revisions", func() {
		it("accepts 40-character git commits only", func() {
			_, err := source.GitRevision("0123456789abcdef0123456789abcdef01234567")
			require.NoError(t, err)

			for _, bad := range []string{"main", "0123", "0123456789ABCDEF0123456789ABCDEF01234567", ""} {
				_, err := source.GitRevision(bad)
				require.Error(t, err, bad)
			}
		})

		it("accepts sha256 digests with or without the algorithm prefix", func() {
			hex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			prefixed, err := source.DigestRevision("sha256:" + hex)
			require.NoError(t, err)
			plain, err := source.DigestRevision(hex)
			require.NoError(t, err)
			assert.Equal(t, prefixed, plain)

			_, err = source.DigestRevision("sha256:abc")
			require.Error(t, err)
		})

		it("validates the revision against the source kind", func() {
			_, err := source.NewRevision(source.KindGit, "sha256:"+repeat("a", 64))
			require.Error(t, err)
			_, err = source.NewRevision(source.KindBlob, "0123456789abcdef0123456789abcdef01234567")
			require.Error(t, err)
		})
	})

	when("deriving cache and context keys", func() {
		it("separates the full snapshot key from derived contexts", func() {
			identity, err := source.NewIdentity(source.KindGit, "https://example/repo", "")
			require.NoError(t, err)
			revision, err := source.GitRevision("0123456789abcdef0123456789abcdef01234567")
			require.NoError(t, err)
			pinned, err := source.NewPinned(identity, revision)
			require.NoError(t, err)

			contextA, err := pinned.ContextKey(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)
			contextB, err := pinned.ContextKey(source.ContextSpec{SubPath: "apps/b"})
			require.NoError(t, err)

			assert.NotEqual(t, contextA, contextB)
			assert.NotEqual(t, pinned.CacheKey(), contextA)
			assert.Contains(t, contextA, pinned.CacheKey())
		})

		it("normalizes equivalent context specs to the same key", func() {
			identity, err := source.NewIdentity(source.KindGit, "https://example/repo", "")
			require.NoError(t, err)
			revision, err := source.GitRevision("0123456789abcdef0123456789abcdef01234567")
			require.NoError(t, err)
			pinned, _ := source.NewPinned(identity, revision)

			a, err := pinned.ContextKey(source.ContextSpec{SubPath: "/apps//a/", Excludes: []string{"vendor/"}})
			require.NoError(t, err)
			b, err := pinned.ContextKey(source.ContextSpec{SubPath: "apps/a", Excludes: []string{"vendor/"}})
			require.NoError(t, err)
			assert.Equal(t, a, b)
		})
	})
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
