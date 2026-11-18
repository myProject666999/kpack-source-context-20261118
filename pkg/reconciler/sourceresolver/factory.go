package sourceresolver

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/pivotal/kpack/pkg/blob"
	"github.com/pivotal/kpack/pkg/git"
	"github.com/pivotal/kpack/pkg/registry"
	"github.com/pivotal/kpack/pkg/secret"
	"github.com/pivotal/kpack/pkg/source"
)

// NewClusterFetcherFactory returns a FetcherFactory that builds source
// fetchers with credentials scoped to the requesting SourceResolver.
func NewClusterFetcherFactory(k8sClient kubernetes.Interface, keychainFactory registry.KeychainFactory, sshTrustUnknownHosts bool) FetcherFactory {
	secretFetcher := &secret.Fetcher{Client: k8sClient}

	return func(ctx context.Context, kind source.Kind, namespace, serviceAccount string) (source.Fetcher, error) {
		switch kind {
		case source.KindGit:
			return git.NewNamespacedSnapshotFetcher(ctx, k8sClient, sshTrustUnknownHosts, namespace, serviceAccount), nil
		case source.KindBlob:
			keychain, err := blob.NewServiceAccountKeychain(ctx, secretFetcher, namespace, serviceAccount, true)
			if err != nil {
				return nil, err
			}
			return &blob.SnapshotFetcher{Keychain: keychain}, nil
		case source.KindRegistry:
			keychain, err := keychainFactory.KeychainForSecretRef(ctx, registry.SecretRef{
				Namespace:      namespace,
				ServiceAccount: serviceAccount,
			})
			if err != nil {
				return nil, err
			}
			return registry.NewSnapshotFetcher(keychain), nil
		default:
			return nil, fmt.Errorf("unsupported source kind %q", kind)
		}
	}
}
