package cmgr

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// registryDeleteTimeout bounds a single registry tag-delete call; prune and
// destroy are best-effort and must never hang an update on a wedged registry.
const registryDeleteTimeout = 30 * time.Second

// registryHTTPClient builds an HTTP client trusting and authenticating with
// the same TLS material dockerd uses for the challenge registry:
// ca.crt / client.cert / client.key under /etc/docker/certs.d/<registry>
// (overridable via CMGR_REGISTRY_CERT_DIR). The registry requires mTLS for
// every operation, so there is no anonymous fallback.
func (m *Manager) registryHTTPClient() (*http.Client, error) {
	certDir := os.Getenv(REGISTRY_CERT_DIR_ENV)
	if certDir == "" {
		certDir = filepath.Join("/etc/docker/certs.d", m.challengeRegistry)
	}

	caPEM, err := os.ReadFile(filepath.Join(certDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("could not read registry CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no usable CA certificates in %s", filepath.Join(certDir, "ca.crt"))
	}

	clientCert, err := tls.LoadX509KeyPair(
		filepath.Join(certDir, "client.cert"),
		filepath.Join(certDir, "client.key"),
	)
	if err != nil {
		return nil, fmt.Errorf("could not load registry client cert: %w", err)
	}

	return &http.Client{
		Timeout: registryDeleteTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{clientCert},
			},
		},
	}, nil
}

// registryDeleteTag removes a tag from the challenge registry so pruned or
// destroyed generations do not accumulate there (content-addressed tags are
// never overwritten, so without this the registry grows one tag per content
// generation forever). imageName must be the registry-qualified reference the
// fork uses everywhere (see instanceImageName).
//
// The delete is issued strictly BY TAG (OCI distribution spec 1.1 tag
// deletion, which zot supports): a manifest-digest delete would remove every
// tag sharing that manifest, and byte-identical images legitimately share
// manifests across seeds. Blob reclamation after untagging is the registry
// GC's job.
//
// Best-effort by contract: callers treat any error as "tag leaked in the
// registry" (recoverable) and must not fail the surrounding operation.
func (m *Manager) registryDeleteTag(imageName string) error {
	repoAndTag, ok := strings.CutPrefix(imageName, m.challengeRegistry+"/")
	if !ok {
		return fmt.Errorf("image %s is not qualified with registry %s", imageName, m.challengeRegistry)
	}
	repo, tag, ok := strings.Cut(repoAndTag, ":")
	if !ok {
		return fmt.Errorf("image %s has no tag", imageName)
	}

	httpClient, err := m.registryHTTPClient()
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", m.challengeRegistry, repo, tag)
	req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		// Already gone — e.g. a shared tuple pruned via another row, or a
		// tag that was never pushed (builder images). Not an error.
		return nil
	case resp.StatusCode == http.StatusMethodNotAllowed:
		return fmt.Errorf("registry %s does not accept tag deletes (405); tag %s leaked", m.challengeRegistry, imageName)
	default:
		return fmt.Errorf("registry delete of %s returned %s", imageName, resp.Status)
	}
}

// registryTagExists reports whether the challenge registry already serves a
// manifest under imageName's tag. It is the guard that keeps the registry
// write-once: a content-addressed tag is pushed at most once, and never over
// whatever a tag already names (see publishImages). A registry that cannot be
// asked is an error, not "absent": pushing on a guess is exactly what the guard
// exists to prevent.
func (m *Manager) registryTagExists(imageName string) (bool, error) {
	repoAndTag, ok := strings.CutPrefix(imageName, m.challengeRegistry+"/")
	if !ok {
		return false, fmt.Errorf("image %s is not qualified with registry %s", imageName, m.challengeRegistry)
	}
	repo, tag, ok := strings.Cut(repoAndTag, ":")
	if !ok {
		return false, fmt.Errorf("image %s has no tag", imageName)
	}

	httpClient, err := m.registryHTTPClient()
	if err != nil {
		return false, err
	}

	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", m.challengeRegistry, repo, tag)
	req, err := http.NewRequestWithContext(m.ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	// Every manifest type a docker push can leave behind: a HEAD without an
	// Accept a registry can satisfy is answered 404 for a tag that exists.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("registry check of %s returned %s", imageName, resp.Status)
	}
}
