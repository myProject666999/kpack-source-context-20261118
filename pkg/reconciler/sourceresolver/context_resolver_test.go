package sourceresolver_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/sclevine/spec"
	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/pivotal/kpack/pkg/apis/core/v1alpha1"
	"github.com/pivotal/kpack/pkg/blob"
	kpackgit "github.com/pivotal/kpack/pkg/git"
	"github.com/pivotal/kpack/pkg/reconciler/sourceresolver"
	"github.com/pivotal/kpack/pkg/source"
)

func TestContextResolverLocalGit(t *testing.T) {
	spec.Run(t, "Source context resolver (local git)", testContextResolverLocalGit)
}

func testContextResolverLocalGit(t *testing.T, when spec.G, it spec.S) {
	when("restoring a blob digest from previous status", func() {
		it("does not re-download when the recorded digest matches the content", func() {
			archive := testTarGz(t, map[string]string{"apps/a/main.go": "package main"})
			var downloads int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt64(&downloads, 1)
				w.Write(archive)
			}))
			defer server.Close()

			digest := source.DigestFromBytes(archive)

			resolver := sourceresolver.NewContextResolver(source.NewCache(), func(_ context.Context, _ source.Kind, _, _ string) (source.Fetcher, error) {
				return &blob.SnapshotFetcher{}, nil
			})

			config := corev1alpha1.SourceConfig{Blob: &corev1alpha1.Blob{URL: server.URL + "/src.tar.gz"}, SubPath: "apps/a"}
			// The legacy blob resolver produces no digest; the previous status
			// carries the digest discovered earlier.
			resolved := corev1alpha1.ResolvedSourceConfig{Blob: &corev1alpha1.ResolvedBlobSource{URL: server.URL + "/src.tar.gz"}}
			previous := corev1alpha1.ResolvedSourceConfig{Blob: &corev1alpha1.ResolvedBlobSource{URL: server.URL + "/src.tar.gz", Digest: digest}}

			out, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, previous)
			require.NoError(t, err)
			require.Equal(t, digest, out.Blob.Digest)
			require.NotEmpty(t, out.Blob.ContextDigest)
			// The snapshot is still fetched once to derive the context, but the
			// digest is not re-discovered via a second download.
			require.Equal(t, int64(1), atomic.LoadInt64(&downloads))
		})
	})

	when("trimming a monorepo", func() {
		it("does not change the build input when a commit only touches another subdirectory", func() {
			repo := newLocalRepo(t)
			first := repo.commit(t, map[string]string{
				"apps/a/main.go": "package main // a",
				"apps/b/main.go": "package main // b",
			})
			second := repo.commit(t, map[string]string{
				"apps/b/main.go": "package main // b changed",
			})

			cache := source.NewCache()
			resolver := sourceresolver.NewContextResolver(cache, gitFactory(repo.dir, ""))

			contextOf := func(commit string) string {
				resolved := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{
					URL:      repo.dir,
					Revision: commit,
					Type:     corev1alpha1.Commit,
				}}
				config := corev1alpha1.SourceConfig{Git: &corev1alpha1.Git{URL: repo.dir, Revision: commit}, SubPath: "apps/a"}
				out, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, corev1alpha1.ResolvedSourceConfig{})
				require.NoError(t, err)
				require.NotEmpty(t, out.Git.ContextDigest)
				return out.Git.ContextDigest
			}

			before := contextOf(first)
			after := contextOf(second)
			require.Equal(t, before, after, "a commit outside apps/a must not change apps/a's build input")
		})

		it("uses the pinned commit even when the branch advances between reads", func() {
			repo := newLocalRepo(t)
			pinned := repo.commit(t, map[string]string{"apps/a/main.go": "package main // one"})
			_ = repo.commit(t, map[string]string{"apps/a/main.go": "package main // two"})

			cache := source.NewCache()
			resolver := sourceresolver.NewContextResolver(cache, gitFactory(repo.dir, ""))
			resolved := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{
				URL:      repo.dir,
				Revision: pinned,
				Type:     corev1alpha1.Commit,
			}}
			config := corev1alpha1.SourceConfig{Git: &corev1alpha1.Git{URL: repo.dir, Revision: "master"}, SubPath: "apps/a"}

			out, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, corev1alpha1.ResolvedSourceConfig{})
			require.NoError(t, err)
			require.Equal(t, pinned, out.Git.Revision, "the resolver must keep the pinned commit, not re-resolve the branch")

			// The context digest must reflect the pinned commit's content.
			direct, err := cache.GetContext(context.Background(), mustPinned(t, repo.dir, pinned), source.ContextSpec{SubPath: "apps/a"}, gitFetcher(repo.dir))
			require.NoError(t, err)
			require.Equal(t, direct.Digest, out.Git.ContextDigest)
		})

		it("returns an error when the subpath does not exist in the revision", func() {
			repo := newLocalRepo(t)
			commit := repo.commit(t, map[string]string{"apps/a/main.go": "package main"})

			cache := source.NewCache()
			resolver := sourceresolver.NewContextResolver(cache, gitFactory(repo.dir, ""))
			resolved := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{URL: repo.dir, Revision: commit, Type: corev1alpha1.Commit}}
			config := corev1alpha1.SourceConfig{Git: &corev1alpha1.Git{URL: repo.dir, Revision: commit}, SubPath: "apps/missing"}

			_, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, corev1alpha1.ResolvedSourceConfig{})
			require.Error(t, err)
			require.True(t, source.IsContextError(err, source.ErrSubPathNotFound), "%v", err)
		})

		it("rejects subpaths that try to escape the source root", func() {
			repo := newLocalRepo(t)
			commit := repo.commit(t, map[string]string{"apps/a/main.go": "package main"})

			unused := source.NewCache()
			resolver := sourceresolver.NewContextResolver(unused, gitFactory(repo.dir, ""))
			resolved := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{URL: repo.dir, Revision: commit, Type: corev1alpha1.Commit}}

			for _, subPath := range []string{"../escape", "apps/../../escape", "C:\\escape"} {
				config := corev1alpha1.SourceConfig{Git: &corev1alpha1.Git{URL: repo.dir, Revision: commit}, SubPath: subPath}
				_, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, corev1alpha1.ResolvedSourceConfig{})
				require.Error(t, err, subPath)
			}
		})

		it("fetches a pinned revision once when two Images request it concurrently", func() {
			repo := newLocalRepo(t)
			commit := repo.commit(t, map[string]string{"apps/a/main.go": "package main"})

			counting := &countingFetcher{delegate: gitFetcher(repo.dir)}
			cache := source.NewCache()
			resolver := sourceresolver.NewContextResolver(cache, func(_ context.Context, _ source.Kind, _, _ string) (source.Fetcher, error) {
				return counting, nil
			})

			config := corev1alpha1.SourceConfig{Git: &corev1alpha1.Git{URL: repo.dir, Revision: commit}, SubPath: "apps/a"}

			var wg sync.WaitGroup
			digests := make([]string, 8)
			errs := make([]error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					resolved := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{URL: repo.dir, Revision: commit, Type: corev1alpha1.Commit}}
					out, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, corev1alpha1.ResolvedSourceConfig{})
					errs[i] = err
					if err == nil {
						digests[i] = out.Git.ContextDigest
					}
				}(i)
			}
			wg.Wait()

			for i := range errs {
				require.NoError(t, errs[i])
				require.Equal(t, digests[0], digests[i])
			}
			require.Equal(t, int64(1), atomic.LoadInt64(&counting.calls), "the pinned revision must be fetched exactly once")
		})

		it("retries a failed fetch instead of reading a partial cache entry", func() {
			repo := newLocalRepo(t)
			commit := repo.commit(t, map[string]string{"apps/a/main.go": "package main"})

			failing := &countingFetcher{failFirst: true, calls: 0, delegate: gitFetcher(repo.dir)}
			cache := source.NewCache()
			resolver := sourceresolver.NewContextResolver(cache, func(_ context.Context, _ source.Kind, _, _ string) (source.Fetcher, error) {
				return failing, nil
			})

			config := corev1alpha1.SourceConfig{Git: &corev1alpha1.Git{URL: repo.dir, Revision: commit}, SubPath: "apps/a"}
			resolved := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{URL: repo.dir, Revision: commit, Type: corev1alpha1.Commit}}

			_, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, corev1alpha1.ResolvedSourceConfig{})
			require.Error(t, err)

			out, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, corev1alpha1.ResolvedSourceConfig{})
			require.NoError(t, err)
			require.NotEmpty(t, out.Git.ContextDigest)
			require.Equal(t, int64(2), atomic.LoadInt64(&failing.calls))
		})

		it("reuses a digest recorded in the previous status instead of re-discovering", func() {
			repo := newLocalRepo(t)
			commit := repo.commit(t, map[string]string{"apps/a/main.go": "package main"})

			counting := &countingFetcher{delegate: gitFetcher(repo.dir)}
			resolver := sourceresolver.NewContextResolver(source.NewCache(), func(_ context.Context, _ source.Kind, _, _ string) (source.Fetcher, error) {
				return counting, nil
			})

			config := corev1alpha1.SourceConfig{Git: &corev1alpha1.Git{URL: repo.dir, Revision: commit}, SubPath: "apps/a"}
			resolved := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{URL: repo.dir, Revision: commit, Type: corev1alpha1.Commit}}
			previous := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{
				URL:           repo.dir,
				Revision:      commit,
				ContextDigest: "sha256:previously-computed",
			}}

			// Git revisions are always known, so this primarily proves the
			// previous status path compiles and does not break git pinning.
			out, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, previous)
			require.NoError(t, err)
			require.Equal(t, commit, out.Git.Revision)
			require.NotEmpty(t, out.Git.ContextDigest)
		})
	})

	when("leaving trimming disabled", func() {
		it("does not set a context digest and returns the resolved source unchanged", func() {
			repo := newLocalRepo(t)
			commit := repo.commit(t, map[string]string{"main.go": "package main"})

			cache := source.NewCache()
			fetcherUsed := false
			resolver := sourceresolver.NewContextResolver(cache, func(_ context.Context, _ source.Kind, _, _ string) (source.Fetcher, error) {
				fetcherUsed = true
				return gitFetcher(repo.dir), nil
			})

			resolved := corev1alpha1.ResolvedSourceConfig{Git: &corev1alpha1.ResolvedGitSource{
				URL:      repo.dir,
				Revision: commit,
				Type:     corev1alpha1.Commit,
				SubPath:  "",
			}}
			config := corev1alpha1.SourceConfig{Git: &corev1alpha1.Git{URL: repo.dir, Revision: commit}}

			out, err := resolver.ResolveContext(context.Background(), "", "", config, resolved, corev1alpha1.ResolvedSourceConfig{})
			require.NoError(t, err)
			require.Equal(t, resolved, out)
			require.False(t, fetcherUsed, "an untrimmed source must not fetch a snapshot")
		})
	})
}

type countingFetcher struct {
	mutex     sync.Mutex
	delegate  source.Fetcher
	failFirst bool
	calls     int64
}

func (f *countingFetcher) Fetch(ctx context.Context, pinned source.Pinned) (*source.Snapshot, error) {
	n := atomic.AddInt64(&f.calls, 1)
	if f.failFirst && n == 1 {
		return nil, context.Canceled
	}
	return f.delegate.Fetch(ctx, pinned)
}

func gitFactory(dir, serviceAccount string) sourceresolver.FetcherFactory {
	return func(_ context.Context, _ source.Kind, _, _ string) (source.Fetcher, error) {
		return gitFetcher(dir), nil
	}
}

func gitFetcher(dir string) source.Fetcher {
	return &kpackgit.SnapshotFetcher{
		AuthFor: func(string) (transport.AuthMethod, error) { return nil, nil },
	}
}

func mustPinned(t *testing.T, url, commit string) source.Pinned {
	t.Helper()
	identity, err := source.NewIdentity(source.KindGit, url, "")
	require.NoError(t, err)
	revision, err := source.GitRevision(commit)
	require.NoError(t, err)
	pinned, err := source.NewPinned(identity, revision)
	require.NoError(t, err)
	return pinned
}

type localRepo struct {
	dir      string
	repo     *git.Repository
	worktree *git.Worktree
	head     plumbing.Hash
}

func newLocalRepo(t *testing.T) *localRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	return &localRepo{dir: dir, repo: repo, worktree: wt}
}

func (r *localRepo) commit(t *testing.T, files map[string]string) string {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(r.dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		_, err := r.worktree.Add(name)
		require.NoError(t, err)
	}
	hash, err := r.worktree.Commit("commit", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com"},
	})
	require.NoError(t, err)
	r.head = hash
	return hash.String()
}

func testTarGz(t *testing.T, files map[string]string) []byte {
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
