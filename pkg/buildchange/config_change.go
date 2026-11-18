package buildchange

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	buildapi "github.com/pivotal/kpack/pkg/apis/build/v1alpha2"
	corev1alpha1 "github.com/pivotal/kpack/pkg/apis/core/v1alpha1"
)

func NewConfigChange(oldConfig, newConfig Config) Change {
	return configChange{
		old: oldConfig,
		new: newConfig,
	}
}

type configChange struct {
	old Config
	new Config
}

type Config struct {
	Env         []corev1.EnvVar             `json:"env,omitempty"`
	Resources   corev1.ResourceRequirements `json:"resources,omitempty"`
	Services    buildapi.Services           `json:"services,omitempty"`
	CNBBindings corev1alpha1.CNBBindings    `json:"cnbBindings,omitempty"`
	Source      corev1alpha1.SourceConfig   `json:"source,omitempty"`
}

func (c configChange) Reason() buildapi.BuildReason { return buildapi.BuildReasonConfig }

func (c configChange) IsBuildRequired() (bool, error) {
	// Revision/digest changes are considered COMMIT changes. Ignore them here
	// as part of the CONFIG change, otherwise every new commit would also be
	// reported as a configuration change.
	oldGitRevision := ""
	newGitRevision := ""
	if c.old.Source.Git != nil {
		oldGitRevision = c.old.Source.Git.Revision
		c.old.Source.Git.Revision = ""
	}
	if c.new.Source.Git != nil {
		newGitRevision = c.new.Source.Git.Revision
		c.new.Source.Git.Revision = ""
	}

	oldBlobDigest := ""
	newBlobDigest := ""
	if c.old.Source.Blob != nil {
		oldBlobDigest = c.old.Source.Blob.Digest
		c.old.Source.Blob.Digest = ""
	}
	if c.new.Source.Blob != nil {
		newBlobDigest = c.new.Source.Blob.Digest
		c.new.Source.Blob.Digest = ""
	}

	oldRegistryDigest := ""
	newRegistryDigest := ""
	if c.old.Source.Registry != nil {
		oldRegistryDigest = c.old.Source.Registry.Digest
		c.old.Source.Registry.Digest = ""
	}
	if c.new.Source.Registry != nil {
		newRegistryDigest = c.new.Source.Registry.Digest
		c.new.Source.Registry.Digest = ""
	}

	// The context digest is likewise a content change, decided by the
	// COMMIT-style comparison, not by CONFIG.
	oldContextDigest := c.old.Source.ContextDigest
	newContextDigest := c.new.Source.ContextDigest
	c.old.Source.ContextDigest = ""
	c.new.Source.ContextDigest = ""

	valid := !equality.Semantic.DeepEqual(c.old, c.new)

	if c.old.Source.Git != nil {
		c.old.Source.Git.Revision = oldGitRevision
	}
	if c.new.Source.Git != nil {
		c.new.Source.Git.Revision = newGitRevision
	}
	if c.old.Source.Blob != nil {
		c.old.Source.Blob.Digest = oldBlobDigest
	}
	if c.new.Source.Blob != nil {
		c.new.Source.Blob.Digest = newBlobDigest
	}
	if c.old.Source.Registry != nil {
		c.old.Source.Registry.Digest = oldRegistryDigest
	}
	if c.new.Source.Registry != nil {
		c.new.Source.Registry.Digest = newRegistryDigest
	}
	c.old.Source.ContextDigest = oldContextDigest
	c.new.Source.ContextDigest = newContextDigest

	return valid, nil
}

func (c configChange) Old() interface{} { return c.old }

func (c configChange) New() interface{} { return c.new }

func (c configChange) Priority() buildapi.BuildPriority { return buildapi.BuildPriorityHigh }
