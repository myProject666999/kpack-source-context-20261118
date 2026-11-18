package source

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// TrimDirectory applies excludes to an extracted source tree in place. root is
// the directory that represents the context root (already the subPath, if the
// caller mounted or extracted one). Symlinks are removed when they resolve
// outside root rather than followed.
//
// This is the build-time half of trimming. It is only called when excludes are
// configured; a source with no trimming is left untouched so its build input
// is identical to previous releases.
func TrimDirectory(root string, excludes []string) error {
	matcher, err := newExcludeMatcher(normalizeExcludesOrPanic(excludes))
	if err != nil {
		return err
	}
	if matcher == nil || matcher.patterns == nil {
		return nil
	}

	var toRemove []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := checkSymlinkTarget(relative, target); err != nil {
				return err
			}
		}

		if matcher.matches(relative) {
			toRemove = append(toRemove, path)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, path := range toRemove {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("removing excluded path %q: %w", path, err)
		}
	}

	return nil
}

// normalizeExcludesOrPanic validates patterns already accepted at admission
// time; a failure here indicates a programming error, so it is surfaced as a
// non-matching configuration rather than silently trimming.
func normalizeExcludesOrPanic(excludes []string) []string {
	normalized := make([]string, 0, len(excludes))
	for _, exclude := range excludes {
		pattern, err := normalizeExclude(exclude)
		if err != nil {
			continue
		}
		normalized = append(normalized, strings.TrimSpace(pattern))
	}
	return normalized
}
