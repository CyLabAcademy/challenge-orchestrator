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

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// registryRequestTimeout bounds a single registry API call (a tag delete
// after a prune, destroy or failed build); none may hang an update on a
// wedged registry.
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

// registryTagExists reports whether the challenge registry already serves a
// manifest under imageName's tag. It is the guard that keeps the registry
// write-once: a content-addressed tag is pushed at most once, and never over
// whatever a tag already names (see executeBuild). The question goes through
// the local daemon, which reaches the registry with the same certs.d material
// and credentials it pushes and pulls with, so a registry dockerd can push to
// is one this can ask. A registry that cannot be asked is an error, not
// "absent": pushing on a guess is exactly what the guard exists to prevent.
func (m *Manager) registryTagExists(imageName string) (bool, error) {
	ctx, cancel := m.controlCtx()
	defer cancel()
	_, err := m.cli.DistributionInspect(ctx, imageName, client.DistributionInspectOptions{EncodedRegistryAuth: m.authString})
	switch {
	case err == nil:
		return true, nil
	case errdefs.IsNotFound(err) || manifestUnknown(err):
		return false, nil
	default:
		return false, err
	}
}

// manifestUnknown recognizes the registry's own "no such manifest" answers
// where the daemon passes them through as text rather than as a not-found.
func manifestUnknown(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"manifest unknown", "no such manifest", "manifest not found"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// registryTagPresent asks the registry itself whether imageName's tag names
// a manifest, over the daemon's own client (registryHTTPClient) rather than
// the local docker daemon registryTagExists goes through: it is the
// question a daemon with no docker daemon asks before it records a build
// someone else pushed (issue #18). 200 is present and 404 absent; anything
// else, a registry that cannot be reached included, is an error, since
// recording a build on a guess is what the check exists to prevent.
func (m *Manager) registryTagPresent(imageName string) (bool, error) {
	code, status, err := m.registryManifestStatus(http.MethodHead, imageName)
	if err != nil {
		return false, err
	}

	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("registry lookup of %s returned %s", imageName, status)
	}
}

// registryManifestStatus makes one distribution API call against the
// manifest imageName's tag names, over the daemon's own client, and
// answers with the status the registry gave: the exchange a HEAD
// (registryTagPresent) and a DELETE (registryDeleteTag) share. The Accept
// header names every manifest type a push can leave behind, so a registry
// that filters by it does not answer 404 for a manifest it holds.
func (m *Manager) registryManifestStatus(method, imageName string) (int, string, error) {
	httpClient, err := m.registryHTTPClient()
	if err != nil {
		return 0, "", err
	}
	req, err := m.registryManifestRequest(method, imageName)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Status, nil
}

// registryHTTPClient is an HTTP client trusting and authenticating with the
// same TLS material dockerd uses for the challenge registry: ca.crt /
// client.cert / client.key under /etc/docker/certs.d/<host> (overridable via
// CMGR_REGISTRY_CERT_DIR). Needed only for what the daemon cannot do -- a
// tag delete -- and best-effort by contract there. Built once and shared,
// but only a client that was built successfully is kept: material that was
// missing or unreadable when first asked for is read again next time, so a
// late cert drop or a rotation heals without a restart.
func (m *Manager) registryHTTPClient() (*http.Client, error) {
	m.registryClientMu.Lock()
	defer m.registryClientMu.Unlock()
	if m.registryClient != nil {
		return m.registryClient, nil
	}
	httpClient, err := m.newRegistryHTTPClient()
	if err != nil {
		return nil, err
	}
	m.registryClient = httpClient
	return httpClient, nil
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
// instanceImageName). The registry's credentials, when configured (the
// CMGR_REGISTRY_USER/TOKEN pair dockerd is handed for pushes and pulls), go
// along as basic auth; the mTLS client identity is on the transport.
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
	if m.registryUser != "" {
		req.SetBasicAuth(m.registryUser, m.registryToken)
	}
	return req, nil
}

// retireRegistryTag takes a tag out of the challenge registry now that no row
// this process can see names its content any more. Only an orchestrator may:
// see AsBuildPlane for why a build plane's database is not the one that gets
// to decide, and registryDeleteTag for the one delete that is not a
// retirement (a build taking back what it had just pushed, which nothing else
// has been told about yet).
//
// Best-effort by contract, as registryDeleteTag is.
func (m *Manager) retireRegistryTag(imageName string) error {
	if m.challengeRegistry == "" || !m.retiresRegistryTags {
		return nil
	}
	return m.registryDeleteTag(imageName)
}

// registryDeleteTag removes a tag from the challenge registry so pruned or
// destroyed generations, and the tags of a build that failed after pushing,
// do not accumulate there (content-addressed tags are never overwritten, so
// without this the registry grows one tag per content generation forever).
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
	code, status, err := m.registryManifestStatus(http.MethodDelete, imageName)
	if err != nil {
		return err
	}

	switch code {
	case http.StatusAccepted:
		return nil
	case http.StatusNotFound:
		// Already gone — e.g. a shared tuple pruned via another row, or a
		// tag that was never pushed (builder images). Not an error.
		return nil
	case http.StatusMethodNotAllowed:
		return fmt.Errorf("registry %s does not accept tag deletes (405); tag %s leaked", m.challengeRegistry, imageName)
	default:
		return fmt.Errorf("registry delete of %s returned %s", imageName, status)
	}
}
