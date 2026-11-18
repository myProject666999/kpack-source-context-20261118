package source_test

import (
	"testing"

	"github.com/sclevine/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pivotal/kpack/pkg/source"
)

func TestContextSpec(t *testing.T) {
	spec.Run(t, "Source context spec", testContextSpec)
}

func testContextSpec(t *testing.T, when spec.G, it spec.S) {
	when("normalizing subpaths", func() {
		it("accepts relative directories and trims noise", func() {
			for input, expected := range map[string]string{
				"":                    "",
				"apps/a":              "apps/a",
				"/apps/a":             "apps/a",
				"apps//a/":            "apps/a",
				"./apps/a":            "apps/a",
				"apps/a/./b/..hidden": "apps/a/b/..hidden",
			} {
				actual, err := source.NormalizeSubPath(input)
				require.NoError(t, err, input)
				assert.Equal(t, expected, actual, input)
			}
		})

		it("accepts backslash separators as separators", func() {
			actual, err := source.NormalizeSubPath(`apps\a`)
			require.NoError(t, err)
			assert.Equal(t, "apps/a", actual)
		})

		it("rejects up-level and absolute escape attempts", func() {
			for _, input := range []string{
				"..",
				"../x",
				"apps/../../x",
				"apps/../x",
				"C:\\x",
				"C:/x",
				`\\server\share`,
			} {
				_, err := source.NormalizeSubPath(input)
				require.Error(t, err, input)
				require.True(t, source.IsContextError(err, source.ErrEscapesRoot), "%s: %v", input, err)
			}
		})
	})

	when("normalizing excludes", func() {
		it("rejects up-level, absolute and empty patterns", func() {
			for _, pattern := range []string{"", "   ", "../secret", "a/../../b", "C:/x", `\\server\share`} {
				err := source.ValidateExcludePattern(pattern)
				require.Error(t, err, pattern)
			}
		})

		it("accepts rooted patterns because a leading slash anchors to the context root", func() {
			require.NoError(t, source.ValidateExcludePattern("/vendor/"))
			require.NoError(t, source.ValidateExcludePattern("**/*.log"))
			require.NoError(t, source.ValidateExcludePattern("a..b"))
		})
	})

	when("normalizing the full spec", func() {
		it("canonicalizes subpath and excludes together", func() {
			actual, err := source.ContextSpec{
				SubPath:  "/apps/a/",
				Excludes: []string{" vendor/ ", "*.log"},
			}.Normalize()
			require.NoError(t, err)
			assert.Equal(t, source.ContextSpec{SubPath: "apps/a", Excludes: []string{"vendor/", "*.log"}}, actual)
		})

		it("reports whether trimming is trivial", func() {
			assert.True(t, source.ContextSpec{}.IsTrivial())
			assert.True(t, source.ContextSpec{SubPath: "."}.IsTrivial())
			assert.False(t, source.ContextSpec{SubPath: "apps/a"}.IsTrivial())
			assert.False(t, source.ContextSpec{Excludes: []string{"*.log"}}.IsTrivial())
		})
	})
}

func TestSelectContext(t *testing.T) {
	spec.Run(t, "Source context selection", testSelectContext)
}

func testSelectContext(t *testing.T, when spec.G, it spec.S) {
	newSnapshot := func(entries ...source.Entry) *source.Snapshot {
		identity, err := source.NewIdentity(source.KindGit, "https://example/repo", "")
		require.NoError(t, err)
		revision, err := source.GitRevision("0123456789abcdef0123456789abcdef01234567")
		require.NoError(t, err)
		return &source.Snapshot{Identity: identity, Revision: revision, Entries: entries, Files: map[string][]byte{}}
	}

	file := func(path, hash string) source.Entry {
		return source.Entry{Path: path, Type: source.EntryFile, Mode: "644", Hash: hash}
	}

	when("trimming to a subpath", func() {
		it("selects only files under the subpath", func() {
			snapshot := newSnapshot(
				file("apps/a/main.go", "a"),
				file("apps/a/pkg/x.go", "b"),
				file("apps/b/main.go", "c"),
				file("README.md", "d"),
			)

			context, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)
			assert.Equal(t, []source.Entry{
				{Path: "main.go", Type: source.EntryFile, Mode: "644", Hash: "a"},
				{Path: "pkg/x.go", Type: source.EntryFile, Mode: "644", Hash: "b"},
			}, context.Entries)
		})

		it("is stable when only siblings change", func() {
			base := newSnapshot(
				file("apps/a/main.go", "a"),
				file("apps/b/main.go", "b"),
			)
			changed := newSnapshot(
				file("apps/a/main.go", "a"),
				file("apps/b/main.go", "changed"),
			)

			contextA, err := base.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)
			contextB, err := changed.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)

			assert.Equal(t, contextA.Digest, contextB.Digest)
		})

		it("changes when content inside the subpath changes", func() {
			base := newSnapshot(file("apps/a/main.go", "a"))
			changed := newSnapshot(file("apps/a/main.go", "changed"))

			contextA, err := base.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)
			contextB, err := changed.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)

			assert.NotEqual(t, contextA.Digest, contextB.Digest)
		})

		it("returns a clear error when the subpath does not exist", func() {
			snapshot := newSnapshot(file("apps/a/main.go", "a"))
			_, err := snapshot.Context(source.ContextSpec{SubPath: "apps/missing"})
			require.Error(t, err)
			assert.True(t, source.IsContextError(err, source.ErrSubPathNotFound))
		})
	})

	when("applying excludes", func() {
		it("removes matching files relative to the subpath", func() {
			snapshot := newSnapshot(
				file("apps/a/main.go", "a"),
				file("apps/a/vendor/lib.go", "b"),
				file("apps/a/debug.log", "c"),
			)

			context, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a", Excludes: []string{"vendor/", "*.log"}})
			require.NoError(t, err)
			assert.Equal(t, []source.Entry{
				{Path: "main.go", Type: source.EntryFile, Mode: "644", Hash: "a"},
			}, context.Entries)
		})

		it("reports an explicit error when excludes empty the context", func() {
			snapshot := newSnapshot(file("apps/a/main.go", "a"))

			_, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a", Excludes: []string{"*"}})
			require.Error(t, err)
			assert.True(t, source.IsContextError(err, source.ErrEmptyContext), "%v", err)
		})

		it("applies excludes at the source root when no subpath is set", func() {
			snapshot := newSnapshot(
				file("main.go", "a"),
				file("notes.log", "b"),
			)

			context, err := snapshot.Context(source.ContextSpec{Excludes: []string{"*.log"}})
			require.NoError(t, err)
			assert.Equal(t, []source.Entry{
				{Path: "main.go", Type: source.EntryFile, Mode: "644", Hash: "a"},
			}, context.Entries)
		})
	})

	when("handling case differences", func() {
		it("rejects paths that differ only by case", func() {
			snapshot := newSnapshot(
				file("apps/a/README", "a"),
				file("apps/a/readme", "b"),
			)

			_, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a"})
			require.Error(t, err)
			assert.True(t, source.IsContextError(err, source.ErrPathConflict), "%v", err)
		})
	})

	when("handling symlinks", func() {
		it("rejects symlinks that escape the source root", func() {
			snapshot := newSnapshot(
				source.Entry{Path: "apps/a/link", Type: source.EntrySymlink, Mode: "120000", Hash: "x"},
			)
			snapshot.Files["apps/a/link"] = []byte("../../../outside")

			_, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a"})
			require.Error(t, err)
			assert.True(t, source.IsContextError(err, source.ErrSymlinkEscape), "%v", err)
		})

		it("allows symlinks that stay inside the source root", func() {
			snapshot := newSnapshot(
				source.Entry{Path: "apps/a/link", Type: source.EntrySymlink, Mode: "120000", Hash: "x"},
				file("apps/a/target.go", "b"),
			)
			snapshot.Files["apps/a/link"] = []byte("./target.go")

			_, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a"})
			require.NoError(t, err)
		})

		it("rejects absolute symlink targets", func() {
			snapshot := newSnapshot(
				source.Entry{Path: "apps/a/link", Type: source.EntrySymlink, Mode: "120000", Hash: "x"},
			)
			snapshot.Files["apps/a/link"] = []byte("/etc/passwd")

			_, err := snapshot.Context(source.ContextSpec{SubPath: "apps/a"})
			require.Error(t, err)
			assert.True(t, source.IsContextError(err, source.ErrSymlinkEscape))
		})
	})
}
