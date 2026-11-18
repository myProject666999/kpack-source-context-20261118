package buildchange

import buildapi "github.com/pivotal/kpack/pkg/apis/build/v1alpha2"

// NewContextChange reports a commit-style change for a source with a trimmed
// context. It is a change only when the context digest differs, so a branch
// advancing outside the Image's subdirectory does not trigger a rebuild.
func NewContextChange(oldRevision, newRevision, oldDigest, newDigest string) Change {
	return contextChange{
		oldRevision: oldRevision,
		newRevision: newRevision,
		oldDigest:   oldDigest,
		newDigest:   newDigest,
	}
}

type contextChange struct {
	oldRevision string
	newRevision string
	oldDigest   string
	newDigest   string
}

func (c contextChange) Reason() buildapi.BuildReason { return buildapi.BuildReasonCommit }

func (c contextChange) IsBuildRequired() (bool, error) {
	return c.oldDigest != c.newDigest, nil
}

// Old and New expose the revisions so logs still show what was compared, while
// the digest is what decides whether a build is required.
func (c contextChange) Old() interface{} {
	return contextChangePayload{Revision: c.oldRevision, ContextDigest: c.oldDigest}
}

func (c contextChange) New() interface{} {
	return contextChangePayload{Revision: c.newRevision, ContextDigest: c.newDigest}
}

func (c contextChange) Priority() buildapi.BuildPriority { return buildapi.BuildPriorityHigh }

type contextChangePayload struct {
	Revision      string `json:"revision"`
	ContextDigest string `json:"contextDigest"`
}
