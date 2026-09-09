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

// registryRequestTimeout bounds a single registry API call (a tag existence
// check before a push, a tag delete after a prune or destroy); none of them
// may hang an update on a wedged registry.
const registryRequestTimeout = 30 * time.Second

// registryEndpoint splits CMGR_REGISTRY into the host dockerd dials and the
// path prefix under it, if any. A namespaced registry (host:5000/ctf) keeps
// its distribution API at https://host:5000/v2/ with "ctf" as the leading
// segment of every repository name -- and dockerd keys certs.d by the host
// alone -- so the two halves are used separately everywhere below.
func (m *Manager) registryEndpoint() (host, prefix string) {
	host, prefix, _ = strings.Cut(m.challengeRegistry, "/")
	return host, strings.Trim(prefix, "/")
}

// registryHTTPClient is an HTTP client trusting and authenticating with the
// same TLS material dockerd uses for the challenge registry: ca.crt /
// client.cert / client.key under /etc/docker/certs.d/<host> (overridable via
// CMGR_REGISTRY_CERT_DIR). The registry requires mTLS for every operation, so
// there is no anonymous fallback. Built once and shared: the check before
// every push and the deletes after a prune all talk to the one registry, and
// a client per call would read and parse the key material and open a fresh
// TLS connection each time.
func (m *Manager) registryHTTPClient() (*http.Client, error) {
	m.registryClientOnce.Do(func() {
		m.registryClient, m.registryClientErr = m.newRegistryHTTPClient()
	})
	return m.registryClient, m.registryClientErr
}

func (m *Manager) newRegistryHTTPClient() (*http.Client, error) {
	certDir := os.Getenv(REGISTRY_CERT_DIR_ENV)
	if certDir == "" {
		host, _ := m.registryEndpoint()
		certDir = filepath.Join("/etc/docker/certs.d", host)
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
		Timeout: registryRequestTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{clientCert},
			},
			IdleConnTimeout: registryRequestTimeout,
		},
	}, nil
}

// registryManifestRequest builds a distribution API request against the
// manifest that imageName's tag names: <method>
// https://<host>/v2/<prefix/><repo>/manifests/<tag>. imageName must be the
// registry-qualified reference the fork uses everywhere (see
// instanceImageName). The registry's credentials, when configured
// (CMGR_REGISTRY_USER/TOKEN, the ones dockerd is handed for pushes and
// pulls), go along as basic auth; the mTLS client identity is on the
// transport.
func (m *Manager) registryManifestRequest(method, imageName string) (*http.Request, error) {
	repoAndTag, ok := strings.CutPrefix(imageName, m.challengeRegistry+"/")
	if !ok {
		return nil, fmt.Errorf("image %s is not qualified with registry %s", imageName, m.challengeRegistry)
	}
	repo, tag, ok := strings.Cut(repoAndTag, ":")
	if !ok {
		return nil, fmt.Errorf("image %s has no tag", imageName)
	}
	host, prefix := m.registryEndpoint()
	if prefix != "" {
		repo = prefix + "/" + repo
	}

	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repo, tag)
	req, err := http.NewRequestWithContext(m.ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	if user := os.Getenv(REGISTRY_USER_ENV); user != "" {
		req.SetBasicAuth(user, os.Getenv(REGISTRY_TOKEN_ENV))
	}
	return req, nil
}

// registryDeleteTag removes a tag from the challenge registry so pruned or
// destroyed generations do not accumulate there (content-addressed tags are
// never overwritten, so without this the registry grows one tag per content
// generation forever).
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
	httpClient, err := m.registryHTTPClient()
	if err != nil {
		return err
	}
	req, err := m.registryManifestRequest(http.MethodDelete, imageName)
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
	httpClient, err := m.registryHTTPClient()
	if err != nil {
		return false, err
	}
	req, err := m.registryManifestRequest(http.MethodHead, imageName)
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
