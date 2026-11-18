// Package source provides revision-pinned source identities, safe context
// trimming, and a concurrency-safe cache for resolved source trees.
package source

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// Kind identifies the mechanism used to obtain source content.
type Kind string

const (
	KindGit      Kind = "git"
	KindBlob     Kind = "blob"
	KindRegistry Kind = "registry"
)

func (k Kind) valid() bool {
	switch k {
	case KindGit, KindBlob, KindRegistry:
		return true
	default:
		return false
	}
}

// Identity identifies a source independent of the revision being built.
//
// Location is the git URL, blob URL, or registry image reference. Variant
// captures source-level extraction options that change the bytes extracted
// from the same location (for example a blob's strip-components). Variant is
// part of the cache key so that differently-extracted copies never alias.
type Identity struct {
	Kind     Kind
	Location string
	Variant  string
}

// NewIdentity validates and constructs a source identity.
func NewIdentity(kind Kind, location, variant string) (Identity, error) {
	if !kind.valid() {
		return Identity{}, fmt.Errorf("invalid source kind %q", kind)
	}
	if strings.TrimSpace(location) == "" {
		return Identity{}, fmt.Errorf("source location must not be empty")
	}
	return Identity{Kind: kind, Location: location, Variant: variant}, nil
}

// Key returns the stable identity portion of a cache key.
func (i Identity) Key() string {
	if i.Variant == "" {
		return string(i.Kind) + ":" + i.Location
	}
	return string(i.Kind) + ":" + i.Location + "#" + i.Variant
}

// Revision is an immutable pointer into a source: a commit hash for git, or
// a content digest for blobs and registry images.
type Revision struct {
	Value string
}

var (
	gitCommitRegex   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	contentDigestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	plainSHA256Regex = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// GitRevision pins a git source to a commit hash.
func GitRevision(commit string) (Revision, error) {
	if !gitCommitRegex.MatchString(commit) {
		return Revision{}, fmt.Errorf("git revision %q must be a 40 character lowercase hex commit hash", commit)
	}
	return Revision{Value: commit}, nil
}

// DigestRevision pins a blob or registry source to a sha256 content digest.
func DigestRevision(digest string) (Revision, error) {
	if plainSHA256Regex.MatchString(digest) {
		digest = "sha256:" + digest
	}
	if !contentDigestRE.MatchString(digest) {
		return Revision{}, fmt.Errorf("content digest %q must be a sha256 digest", digest)
	}
	return Revision{Value: digest}, nil
}

// NewRevision validates a revision against the source kind.
func NewRevision(kind Kind, value string) (Revision, error) {
	switch kind {
	case KindGit:
		return GitRevision(value)
	case KindBlob, KindRegistry:
		return DigestRevision(value)
	default:
		return Revision{}, fmt.Errorf("invalid source kind %q", kind)
	}
}

// Pinned is a source identity tied to an immutable revision. Once pinned, no
// build step may re-resolve a branch name or tag.
type Pinned struct {
	Identity Identity
	Revision Revision
}

// NewPinned constructs a validated pinned source.
func NewPinned(identity Identity, revision Revision) (Pinned, error) {
	if _, err := NewRevision(identity.Kind, revision.Value); err != nil {
		return Pinned{}, err
	}
	return Pinned{Identity: identity, Revision: revision}, nil
}

// CacheKey identifies the full source content for this pinned revision.
func (p Pinned) CacheKey() string {
	return p.Identity.Key() + "@" + p.Revision.Value
}

// ContextKey identifies the derived context for this pinned revision and
// trimming parameters. Two Images that request the same trimming parameters
// share a derived context; different parameters never alias.
func (p Pinned) ContextKey(spec ContextSpec) (string, error) {
	normalized, err := spec.Normalize()
	if err != nil {
		return "", err
	}
	return p.CacheKey() + "|" + normalized.Key(), nil
}

// DigestFromBytes returns the canonical sha256 digest for content.
func DigestFromBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("sha256:%x", sum)
}
