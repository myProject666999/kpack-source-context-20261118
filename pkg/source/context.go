package source

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ContextErrorKind classifies invalid context requests so callers (and the
// webhook) can return a precise message instead of silently building an empty
// or escaped tree.
type ContextErrorKind string

const (
	ErrEscapesRoot      ContextErrorKind = "EscapesSourceRoot"
	ErrSubPathNotFound  ContextErrorKind = "SubPathNotFound"
	ErrEmptyContext     ContextErrorKind = "EmptyContext"
	ErrPathConflict     ContextErrorKind = "PathConflict"
	ErrSymlinkEscape    ContextErrorKind = "SymlinkEscapesSourceRoot"
	ErrInvalidExclude   ContextErrorKind = "InvalidExclude"
	ErrNotAFileOrDir    ContextErrorKind = "NotAFileOrDirectory"
	ErrContextNotPinned ContextErrorKind = "ContextNotPinned"
)

// ContextError is returned for a rejected context request.
type ContextError struct {
	Kind   ContextErrorKind
	Detail string
}

func (e *ContextError) Error() string {
	if e.Detail == "" {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Detail
}

func contextError(kind ContextErrorKind, format string, args ...interface{}) error {
	return &ContextError{Kind: kind, Detail: fmt.Sprintf(format, args...)}
}

// IsContextError reports whether err is a ContextError of the given kind.
func IsContextError(err error, kind ContextErrorKind) bool {
	e, ok := err.(*ContextError)
	return ok && e.Kind == kind
}

// ContextSpec is the trimming applied to a pinned source to produce an
// Image's build context.
//
// SubPath is a directory inside the source root. It must be relative, must not
// contain "." or ".." components, and must not be absolute. Excludes are
// gitignore-style patterns matched against paths relative to the context root
// (that is, relative to SubPath once trimmed); a pattern beginning with "/" is
// anchored to the context root, as in .gitignore, and is not a filesystem
// absolute path. Excludes may be nil.
type ContextSpec struct {
	SubPath  string
	Excludes []string
}

var (
	windowsAbsolutePath = regexp.MustCompile(`^[a-zA-Z]:`)
)

// Normalize validates the spec and returns its canonical form. A nil result is
// never returned on success.
func (c ContextSpec) Normalize() (ContextSpec, error) {
	subPath, err := NormalizeSubPath(c.SubPath)
	if err != nil {
		return ContextSpec{}, err
	}

	excludes := make([]string, 0, len(c.Excludes))
	for _, raw := range c.Excludes {
		pattern, err := normalizeExclude(raw)
		if err != nil {
			return ContextSpec{}, err
		}
		excludes = append(excludes, pattern)
	}

	return ContextSpec{SubPath: subPath, Excludes: excludes}, nil
}

// IsTrivial reports whether the spec leaves the source tree untouched.
func (c ContextSpec) IsTrivial() bool {
	normalized, err := c.Normalize()
	if err != nil {
		return false
	}
	return normalized.SubPath == "" && len(normalized.Excludes) == 0
}

// Key returns a stable key for the canonicalized spec. Two specs with the same
// key produce identical contexts.
func (c ContextSpec) Key() string {
	normalized, err := c.Normalize()
	if err != nil {
		// Invalid specs are never used to build a cache key; encode the error
		// deterministically so a bug cannot alias two different requests.
		return "invalid:" + err.Error()
	}
	payload := struct {
		SubPath  string   `json:"subPath"`
		Excludes []string `json:"excludes,omitempty"`
	}{SubPath: normalized.SubPath, Excludes: normalized.Excludes}

	encoded, _ := json.Marshal(payload)
	return DigestFromBytes(encoded)
}

// NormalizeSubPath validates a user-supplied subdirectory. Up-level ("..")
// components and Windows drive/UNC paths are rejected rather than cleaned,
// because cleaning would change the meaning of the request and could escape
// the source root.
//
// A single leading "/" is treated as root-relative, not filesystem-absolute:
// kpack has always accepted values such as "/go" as an opaque subdirectory,
// and stripping the slash cannot escape the root. Drive-letter and UNC forms
// are rejected because they denote a different filesystem root.
func NormalizeSubPath(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	if windowsAbsolutePath.MatchString(raw) || strings.HasPrefix(raw, `\\`) {
		return "", contextError(ErrEscapesRoot, "subPath %q must be relative to the source root", raw)
	}

	// Treat Windows separators as separators so a spec is validated the same
	// way regardless of where it was authored.
	path := strings.ReplaceAll(raw, `\`, "/")

	components := make([]string, 0, len(strings.Split(path, "/")))
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		switch component {
		case "", ".":
			continue
		case "..":
			return "", contextError(ErrEscapesRoot, "subPath %q contains an up-level '..' component", raw)
		default:
			components = append(components, component)
		}
	}

	return strings.Join(components, "/"), nil
}

func normalizeExclude(raw string) (string, error) {
	if err := ValidateExcludePattern(raw); err != nil {
		return "", err
	}
	// Normalize only surrounding whitespace; the matcher needs the leading "/"
	// anchor intact.
	return strings.TrimSpace(raw), nil
}

// ValidateExcludePattern validates a single exclude pattern. Patterns are
// gitignore-style and relative to the context root; filesystem-absolute paths
// and up-level components are rejected so a pattern can never reference
// content outside the source root.
func ValidateExcludePattern(raw string) error {
	pattern := strings.TrimSpace(raw)
	if pattern == "" {
		return contextError(ErrInvalidExclude, "exclude patterns must not be empty")
	}
	// A leading "/" anchors the pattern at the context root; it is not a
	// filesystem absolute path. But a drive letter or UNC prefix is an
	// absolute filesystem path and is always rejected.
	if windowsAbsolutePath.MatchString(pattern) || strings.HasPrefix(pattern, `\\`) {
		return contextError(ErrInvalidExclude, "exclude pattern %q must be relative to the context root", raw)
	}

	// A ".." path component would reference content outside the context root.
	// A literal ".." inside a glob (for example "a..b") is fine.
	for _, component := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		if component == ".." {
			return contextError(ErrInvalidExclude, "exclude pattern %q contains an up-level '..' component", raw)
		}
	}

	return nil
}

// EntryType is the kind of a selected context entry.
type EntryType string

const (
	EntryFile    EntryType = "file"
	EntrySymlink EntryType = "symlink"
	EntryDir     EntryType = "dir"
	EntryGitlink EntryType = "gitlink"
)

// Entry is one path in a selected context. Hash is the content identity used
// by the source: a git object ID for git trees, or a sha256 digest of the
// content for extracted archives.
type Entry struct {
	Path string
	Type EntryType
	Mode string
	Hash string
}

// DigestEntries computes a deterministic digest over the selected entries.
// Entries are sorted by path so traversal order never affects the result.
func DigestEntries(entries []Entry) string {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var builder strings.Builder
	for _, entry := range sorted {
		// Length-prefixing prevents path/mode/hash boundary confusion.
		fmt.Fprintf(&builder, "%s\x00%d\x00%s\x00%s\x00%s\n", entry.Type, len(entry.Path), entry.Path, entry.Mode, entry.Hash)
	}
	return DigestFromBytes([]byte(builder.String()))
}

// selectedEntry pairs a context-relative entry with its path in the source
// root, so callers can recover content stored under the source path.
type selectedEntry struct {
	Entry
	SourcePath string
}

// selectEntries applies subpath/exclude trimming to a candidate set and
// validates it. Candidates are paths relative to the source root using "/".
// It returns error kinds for missing subtrees, empty results, escaping paths
// and case-insensitive collisions.
func selectEntries(candidates []Entry, spec ContextSpec) ([]selectedEntry, error) {
	normalized, err := spec.Normalize()
	if err != nil {
		return nil, err
	}

	matcher, err := newExcludeMatcher(normalized.Excludes)
	if err != nil {
		return nil, err
	}

	prefix := ""
	if normalized.SubPath != "" {
		prefix = normalized.SubPath + "/"
	}

	foundSubPath := normalized.SubPath == ""
	selected := make([]selectedEntry, 0, len(candidates))
	seenFold := map[string]string{}

	for _, candidate := range candidates {
		path := candidate.Path
		if path == "" || strings.HasPrefix(path, "/") {
			return nil, contextError(ErrEscapesRoot, "source entry %q is not a relative path", path)
		}
		if hasUpLevelComponent(path) {
			return nil, contextError(ErrEscapesRoot, "source entry %q escapes the source root", path)
		}

		if normalized.SubPath != "" {
			if path == normalized.SubPath {
				foundSubPath = true
				if candidate.Type != EntryDir && candidate.Type != EntryFile && candidate.Type != EntrySymlink {
					return nil, contextError(ErrNotAFileOrDir, "subPath %q is not a file or directory", normalized.SubPath)
				}
				// A file selected directly as the subpath is itself the
				// context root; keep it under a stable relative name.
				if candidate.Type != EntryDir {
					relative := baseName(normalized.SubPath)
					if err := checkFold(seenFold, relative); err != nil {
						return nil, err
					}
					if !matcher.matches(relative) {
						selected = append(selected, selectedEntry{
							Entry:      Entry{Path: relative, Type: candidate.Type, Mode: candidate.Mode, Hash: candidate.Hash},
							SourcePath: path,
						})
					}
				}
				continue
			}
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			foundSubPath = true
		}

		relative := strings.TrimPrefix(path, prefix)
		if relative == "" {
			continue
		}
		if candidate.Type == EntryDir {
			// Directories carry no content in git trees and are implied by
			// their files; excluding a directory is handled by matching its
			// children below. An explicitly excluded directory contributes
			// nothing.
			continue
		}
		if matcher.matches(relative) {
			continue
		}
		if err := checkFold(seenFold, relative); err != nil {
			return nil, err
		}
		selected = append(selected, selectedEntry{
			Entry:      Entry{Path: relative, Type: candidate.Type, Mode: candidate.Mode, Hash: candidate.Hash},
			SourcePath: path,
		})
	}

	if !foundSubPath {
		return nil, contextError(ErrSubPathNotFound, "subPath %q does not exist in the source revision", normalized.SubPath)
	}
	if len(selected) == 0 {
		if normalized.SubPath != "" {
			return nil, contextError(ErrEmptyContext, "subPath %q contains no files after applying exclude rules", normalized.SubPath)
		}
		return nil, contextError(ErrEmptyContext, "exclude rules removed every file from the source")
	}

	return selected, nil
}

func checkFold(seen map[string]string, path string) error {
	folded := strings.ToLower(path)
	if previous, ok := seen[folded]; ok && previous != path {
		return contextError(ErrPathConflict, "paths %q and %q differ only by case; the context would be ambiguous on case-insensitive filesystems", previous, path)
	}
	seen[folded] = path
	return nil
}

func hasUpLevelComponent(path string) bool {
	for _, component := range strings.Split(path, "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

func baseName(path string) string {
	components := strings.Split(path, "/")
	return components[len(components)-1]
}
