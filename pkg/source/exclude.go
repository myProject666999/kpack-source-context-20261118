package source

import (
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

// excludeMatcher applies gitignore-style patterns to context-relative paths.
// Matching a directory pattern also matches everything beneath it, and an
// excluded parent implicitly excludes its children even when the pattern was
// written with a trailing slash.
type excludeMatcher struct {
	patterns *ignore.GitIgnore
}

func newExcludeMatcher(patterns []string) (*excludeMatcher, error) {
	if len(patterns) == 0 {
		return &excludeMatcher{}, nil
	}
	return &excludeMatcher{patterns: ignore.CompileIgnoreLines(patterns...)}, nil
}

func (m *excludeMatcher) matches(relativePath string) bool {
	if m == nil || m.patterns == nil {
		return false
	}

	matchPath := strings.TrimPrefix(relativePath, "./")
	return m.patterns.MatchesPath(matchPath)
}
