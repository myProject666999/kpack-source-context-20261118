package v1alpha1_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/pivotal/kpack/pkg/apis/core/v1alpha1"
)

func TestSourceConfigContextValidation(t *testing.T) {
	valid := func() corev1alpha1.SourceConfig {
		return corev1alpha1.SourceConfig{
			Git: &corev1alpha1.Git{URL: "https://example/repo", Revision: "main"},
		}
	}

	t.Run("accepts a valid subPath and excludes", func(t *testing.T) {
		config := valid()
		config.SubPath = "apps/a"
		config.Excludes = []string{"vendor/", "*.log"}
		require.Nil(t, config.Validate(context.Background()))
	})

	t.Run("rejects up-level subpaths", func(t *testing.T) {
		for _, subPath := range []string{"..", "../escape", "apps/../../escape"} {
			config := valid()
			config.SubPath = subPath
			err := config.Validate(context.Background())
			require.NotNil(t, err, subPath)
			require.Contains(t, err.Error(), "subPath")
		}
	})

	t.Run("rejects drive-letter and UNC subpaths", func(t *testing.T) {
		for _, subPath := range []string{"C:\\escape", `\\server\share`} {
			config := valid()
			config.SubPath = subPath
			require.NotNil(t, config.Validate(context.Background()), subPath)
		}
	})

	t.Run("rejects escaping exclude patterns", func(t *testing.T) {
		for _, pattern := range []string{"../secret", "a/../../b", "C:/escape"} {
			config := valid()
			config.Excludes = []string{pattern}
			err := config.Validate(context.Background())
			require.NotNil(t, err, pattern)
			require.Contains(t, err.Error(), "excludes")
		}
	})

	t.Run("accepts an empty exclude list", func(t *testing.T) {
		config := valid()
		config.Excludes = nil
		require.Nil(t, config.Validate(context.Background()))
	})
}
