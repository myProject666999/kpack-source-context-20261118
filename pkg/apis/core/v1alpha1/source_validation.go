package v1alpha1

import (
	"context"
	"fmt"

	"knative.dev/pkg/apis"

	"github.com/pivotal/kpack/pkg/apis/validate"
	"github.com/pivotal/kpack/pkg/source"
)

func (s *SourceConfig) Validate(ctx context.Context) *apis.FieldError {
	sources := make([]string, 0, 3)
	if s.Git != nil {
		sources = append(sources, "git")
	}
	if s.Blob != nil {
		sources = append(sources, "blob")
	}
	if s.Registry != nil {
		sources = append(sources, "registry")
	}

	if len(sources) == 0 {
		return apis.ErrMissingOneOf("git", "blob", "registry")
	}

	if len(sources) != 1 {
		return apis.ErrMultipleOneOf(sources...)
	}

	if err := validateContext(s.SubPath, s.Excludes); err != nil {
		return err
	}

	return (s.Git.Validate(ctx).ViaField("git")).
		Also(s.Blob.Validate(ctx).ViaField("blob")).
		Also(s.Registry.Validate(ctx).ViaField("registry"))
}

// validateContext rejects trimming inputs that could escape the source root.
// It is enforced at admission time for Build and Image as well as when a
// SourceResolver is reconciled, so an invalid context never reaches a build.
func validateContext(subPath string, excludes []string) *apis.FieldError {
	if _, err := source.NormalizeSubPath(subPath); err != nil {
		return apis.ErrInvalidValue(subPath, "subPath", err.Error())
	}

	for i, exclude := range excludes {
		if err := source.ValidateExcludePattern(exclude); err != nil {
			return apis.ErrInvalidValue(exclude, fmt.Sprintf("excludes[%d]", i), err.Error())
		}
	}

	return nil
}

func (g *Git) Validate(ctx context.Context) *apis.FieldError {
	if g == nil {
		return nil
	}

	return validate.FieldNotEmpty(g.URL, "url").
		Also(validate.FieldNotEmpty(g.Revision, "revision"))
}

func (b *Blob) Validate(ctx context.Context) *apis.FieldError {
	if b == nil {
		return nil
	}

	if b.Auth != "" && b.Auth != "helper" && b.Auth != "secret" {
		return apis.ErrInvalidValue(b.Auth, "auth", "must be one of '', 'helper', or 'secret'")
	}

	fieldError := validate.FieldNotEmpty(b.URL, "url").
		Also(validate.StripComponents(b.StripComponents))

	return fieldError
}

func (r *Registry) Validate(ctx context.Context) *apis.FieldError {
	if r == nil {
		return nil
	}

	return validate.Image(r.Image)
}
