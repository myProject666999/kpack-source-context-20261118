package source_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pivotal/kpack/pkg/source"
)

func TestTrimDirectory(t *testing.T) {
	write := func(t *testing.T, root, name, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	t.Run("removes excluded files but keeps the rest", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "main.go", "package main")
		write(t, root, "vendor/lib.go", "package vendor")
		write(t, root, "debug.log", "noise")

		require.NoError(t, source.TrimDirectory(root, []string{"vendor/", "*.log"}))

		require.FileExists(t, filepath.Join(root, "main.go"))
		require.NoFileExists(t, filepath.Join(root, "vendor", "lib.go"))
		require.NoFileExists(t, filepath.Join(root, "debug.log"))
	})

	t.Run("is a no-op with no excludes", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "main.go", "package main")
		require.NoError(t, source.TrimDirectory(root, nil))
		require.FileExists(t, filepath.Join(root, "main.go"))
	})

	t.Run("rejects symlinks that escape the root", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		write(t, outside, "secret", "s")
		require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")))

		err := source.TrimDirectory(root, []string{"nomatch"})
		require.Error(t, err)
		require.True(t, source.IsContextError(err, source.ErrSymlinkEscape), "%v", err)
	})
}
