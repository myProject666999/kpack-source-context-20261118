package blob

import (
	"context"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	buildapi "github.com/pivotal/kpack/pkg/apis/build/v1alpha2"
	"github.com/pivotal/kpack/pkg/secret"
)

// ServiceAccountKeychain resolves blob credentials from the secrets attached
// to a SourceResolver's service account. It mirrors the annotations honored by
// the build pod (kpack.io/blob) so resolution and the build agree.
type ServiceAccountKeychain struct {
	secrets   []*corev1.Secret
	useHelper bool
}

// NewServiceAccountKeychain reads a service account's secrets, surfacing a
// missing service account as "no credentials" as the git keychain does.
func NewServiceAccountKeychain(ctx context.Context, fetcher *secret.Fetcher, namespace, serviceAccount string, useHelper bool) (*ServiceAccountKeychain, error) {
	secrets, err := fetcher.SecretsForServiceAccount(ctx, serviceAccount, namespace)
	if err != nil && !k8serrors.IsNotFound(err) {
		return nil, err
	} else if k8serrors.IsNotFound(err) {
		return &ServiceAccountKeychain{useHelper: useHelper}, nil
	}
	return &ServiceAccountKeychain{secrets: secrets, useHelper: useHelper}, nil
}

// Resolve implements Keychain.
func (k *ServiceAccountKeychain) Resolve(blobURL string) (string, map[string]string, error) {
	for _, s := range k.secrets {
		domain, ok := s.Annotations[buildapi.BlobSecretAnnotationPrefix]
		if !ok || domain == "" {
			continue
		}
		if !matchesBlobDomain(blobURL, domain) {
			continue
		}
		header, err := blobAuthHeader(s)
		if err != nil {
			return "", nil, err
		}
		return header, nil, nil
	}

	if k.useHelper {
		return DefaultKeychain.Resolve(blobURL)
	}
	return "", nil, nil
}

func matchesBlobDomain(blobURL, domain string) bool {
	// The annotation may be a bare domain or a scheme-prefixed URL, matching
	// the historical matching rules used by the build pod.
	return domain == blobURL || containsHost(blobURL, domain)
}

func containsHost(blobURL, domain string) bool {
	// Intentionally permissive: an annotation such as "blobstore.com" should
	// match https://blobstore.com/....
	return len(domain) > 0 && (hasPrefix(blobURL, domain) ||
		hasPrefix(blobURL, "https://"+domain) ||
		hasPrefix(blobURL, "http://"+domain))
}

func hasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}

func blobAuthHeader(s *corev1.Secret) (string, error) {
	authorization := string(s.Data["authorization"])
	if authorization != "" {
		return authorization, nil
	}
	bearer := string(s.Data["bearer"])
	if bearer != "" {
		return "Bearer " + bearer, nil
	}
	username := string(s.Data[corev1.BasicAuthUsernameKey])
	password := string(s.Data[corev1.BasicAuthPasswordKey])
	if username != "" || password != "" {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password))), nil
	}
	return "", fmt.Errorf("blob secret %q has no usable credentials", s.Name)
}
