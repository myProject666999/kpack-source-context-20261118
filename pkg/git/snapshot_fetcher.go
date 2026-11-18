package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/pivotal/kpack/pkg/source"
)

// SnapshotFetcher materializes a pinned git revision into a full source
// snapshot. Object IDs from the git tree are used as content hashes, so two
// fetches of the same commit yield the same context digest even when obtained
// from different clones. Symlink targets and file contents are captured so a
// derived context can be validated and materialized without another fetch.
//
// Auth is resolved per call because credentials are namespaced to the
// SourceResolver's service account.
type SnapshotFetcher struct {
	AuthFor func(url string) (transport.AuthMethod, error)
}

// Discover implements source.DiscoveringFetcher. Branch and tag resolution is
// intentionally not supported here: discovery would reintroduce floating
// revisions. Callers must pin the git revision to a commit hash first.
func (f *SnapshotFetcher) Discover(_ context.Context, _ source.Identity) (*source.Snapshot, error) {
	return nil, fmt.Errorf("git sources must be pinned to a commit hash before snapshotting")
}

// Fetch implements source.Fetcher.
func (f *SnapshotFetcher) Fetch(ctx context.Context, pinned source.Pinned) (*source.Snapshot, error) {
	if pinned.Identity.Kind != source.KindGit {
		return nil, fmt.Errorf("git snapshot fetcher cannot fetch kind %q", pinned.Identity.Kind)
	}

	auth, err := f.AuthFor(pinned.Identity.Location)
	if err != nil {
		return nil, fmt.Errorf("resolving git credentials: %w", err)
	}

	repository, cleanup, err := cloneBare(ctx, pinned.Identity.Location, pinned.Revision.Value, auth)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	commit, err := repository.CommitObject(plumbing.NewHash(pinned.Revision.Value))
	if err != nil {
		return nil, fmt.Errorf("commit %q not found after fetch: %w", pinned.Revision.Value, err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("reading tree for commit %q: %w", pinned.Revision.Value, err)
	}

	snapshot := &source.Snapshot{
		Identity: pinned.Identity,
		Revision: pinned.Revision,
		Files:    map[string][]byte{},
	}

	// Walk every file so snapshots are complete before they enter the cache.
	// A partial walk returns an error and is never published.
	err = tree.Files().ForEach(func(file *object.File) error {
		entry, content, err := snapshotEntryForFile(repository, file)
		if err != nil {
			return err
		}
		snapshot.Entries = append(snapshot.Entries, entry)
		if content != nil {
			snapshot.Files[entry.Path] = content
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading files for commit %q: %w", pinned.Revision.Value, err)
	}

	sort.Slice(snapshot.Entries, func(i, j int) bool {
		return snapshot.Entries[i].Path < snapshot.Entries[j].Path
	})

	return snapshot, nil
}

func cloneBare(ctx context.Context, url, revision string, auth transport.AuthMethod) (*gogit.Repository, func(), error) {
	dir, err := os.MkdirTemp("", "kpack-git-snapshot")
	if err != nil {
		return nil, nil, fmt.Errorf("creating temporary clone directory: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	repository, err := gogit.PlainInit(dir, true)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("initializing repository: %w", err)
	}

	_, err = repository.CreateRemote(&gogitconfig.RemoteConfig{
		Name: defaultRemote,
		URLs: []string{url},
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating remote: %w", err)
	}

	// Pin the fetch to the exact commit so a branch advancing between reads
	// cannot change which content this snapshot represents. Some transports
	// (notably the local file transport) reject exact-SHA refspecs, so fall
	// back to fetching all refs and then verifying the commit is present.
	spec := fmt.Sprintf("%s:%s", revision, revision)
	err = repository.FetchContext(ctx, &gogit.FetchOptions{
		RemoteName: defaultRemote,
		Auth:       auth,
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(spec)},
		Depth:      1,
	})
	if err != nil && !strings.Contains(err.Error(), "does not support exact SHA1 refspec") {
		cleanup()
		return nil, nil, fmt.Errorf("fetching %q: %w", revision, err)
	}
	if err != nil {
		err = repository.FetchContext(ctx, &gogit.FetchOptions{
			RemoteName: defaultRemote,
			Auth:       auth,
			RefSpecs:   []gogitconfig.RefSpec{"+refs/heads/*:refs/remotes/" + defaultRemote + "/*", "+refs/tags/*:refs/tags/*"},
		})
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("fetching %q: %w", revision, err)
		}
	}

	// The fallback path may fetch more history than needed; the commit must
	// still exist for the pinned content to be available.
	if _, err := repository.CommitObject(plumbing.NewHash(revision)); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("commit %q not found after fetch: %w", revision, err)
	}

	return repository, cleanup, nil
}

func snapshotEntryForFile(repository *gogit.Repository, file *object.File) (source.Entry, []byte, error) {
	entry := source.Entry{
		Path: filepath.ToSlash(file.Name),
		Hash: file.Hash.String(),
		Mode: file.Mode.String(),
	}

	switch file.Mode {
	case filemode.Symlink, filemode.Executable, filemode.Regular:
		content, err := readBlob(file)
		if err != nil {
			return source.Entry{}, nil, err
		}
		entry.Type = source.EntryFile
		if file.Mode == filemode.Symlink {
			entry.Type = source.EntrySymlink
		}
		return entry, content, nil
	default:
		// Submodules (gitlinks) appear as directory-like entries with an
		// object ID but no content in this repository.
		entry.Type = source.EntryGitlink
		return entry, nil, nil
	}
}

func readBlob(file *object.File) ([]byte, error) {
	reader, err := file.Reader()
	if err != nil {
		return nil, fmt.Errorf("opening blob %q: %w", file.Name, err)
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading blob %q: %w", file.Name, err)
	}
	return content, nil
}
