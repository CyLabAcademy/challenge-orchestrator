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
// Retried while it fails without an answer, as the direct exchanges are: a
// build resolves every image against the registry before it builds anything
// and pushes through this, so a blip here fails a build rather than a
// challenge, and on the same odds (see registryAttempts).
func (m *Manager) registryTagExists(imageName string) (bool, error) {
	var present bool
	err := m.retryRegistry(fmt.Sprintf("asking the daemon for %s in the registry", imageName), func() error {
		ctx, cancel := m.controlCtx()
		defer cancel()
		_, err := m.cli.DistributionInspect(ctx, imageName, client.DistributionInspectOptions{EncodedRegistryAuth: m.authString})
		switch {
		case err == nil:
			present = true
			return nil
		case errdefs.IsNotFound(err) || manifestUnknown(err):
			// An answer, and the one that means "not pushed yet".
			present = false
			return nil
		default:
			return err
		}
	})
	return present, err
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
// Retried while it fails without an answer: this is asked once per image of
// every build of every hand-over, and one that goes unanswered refuses the
// whole challenge (see registryAttempts).
func (m *Manager) registryTagPresent(imageName string) (bool, error) {
	var code int
	var status string
	err := m.retryRegistry(fmt.Sprintf("asking the registry for %s", imageName), func() error {
		var err error
		code, status, err = m.registryManifestStatus(http.MethodHead, imageName)
		return err
	})
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

const (
	// registryAttempts is how many times a registry exchange that failed
	// without an answer is tried. A hand-over asks the registry once per
	// image and an event is thousands of images, so at those odds a blip
	// that is not retried is not unlikely but routine -- and it refuses a
	// whole challenge, since a question that cannot be asked is an error
	// rather than "absent" (see registryTagPresent).
	//
	// Three, because this is for a connection that dropped or a registry
	// restarting, not for one that is down: a registry that is really gone
	// should fail the operation while an operator is still watching, not
	// minutes later.
	registryAttempts = 3
	// registryRetryDelay is the wait before the second attempt, doubled for
	// the third. Short: the failures this covers are a reset connection or a
	// process coming back, and the request timeout already bounds the slow
	// case.
	registryRetryDelay = 500 * time.Millisecond
)

// registryRecovery is what an operator does about a registry that could not
// be reached, appended to the errors that abort an operation over it.
//
// It says the same thing for every one of them, because the same thing is
// true of every one of them: an operation that could not reach the registry
// recorded nothing, and re-running it once the registry answers is the whole
// recovery. Nothing drifts while the registry is down -- cork can leave a tag
// no row names, never a row naming a tag that is not there -- so there is no
// state to reconcile afterwards and no repair step to look up. Saying so at
// the point of failure is cheaper than the operator finding out.
const registryRecovery = "nothing was recorded. When the registry is up again, retry the same command."

// registryLeak is the other half, for the deletes that are best-effort: the
// operation itself succeeded and re-running it would do nothing, so the
// advice has to be the opposite one. What is left is a tag no row names,
// which nothing will serve and the registry's own garbage collection
// reclaims. An operator told only "could not remove" reasonably wonders
// whether the destroy took.
const registryLeak = "nothing to do. The tag is left in the registry for its garbage collection to reclaim."

// retryRegistry runs a registry exchange again while it fails without an
// answer, and returns what the last attempt gave.
//
// Only a failure to get an answer at all is retried, which is why exchange
// returns an error rather than a status: a status the registry returned is an
// answer -- 404 included, which is what "not present" is -- so an exchange
// that got one reports nil and keeps its own result. The context being done
// is not retried either: the process is shutting down.
func (m *Manager) retryRegistry(what string, exchange func() error) error {
	delay := registryRetryDelay
	for attempt := 1; ; attempt++ {
		err := exchange()
		if err == nil || attempt == registryAttempts || m.ctx.Err() != nil {
			return err
		}
		m.log.warnf("%s failed (attempt %d of %d): %s; retrying in %s",
			what, attempt, registryAttempts, err, delay)
		select {
		case <-time.After(delay):
		case <-m.ctx.Done():
			return err
		}
		delay *= 2
	}
}

// registryManifestStatus makes one distribution API call against the
// manifest imageName's tag names, over the daemon's own client, and
// answers with the status the registry gave: the exchange a HEAD
// (registryTagPresent) and a DELETE (registryDeleteTag) share. The Accept
// header names every manifest type a push can leave behind, so a registry
// that filters by it does not answer 404 for a manifest it holds.
//
// One call, never retried here. Whether a failure is worth asking again is
// the caller's to decide and the two callers differ: a HEAD that fails
// refuses a hand-over and is retried, while a DELETE that fails leaks a tag
// and is not (see registryDeleteTag).
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
	if m.challengeRegistry == "" || m.buildPlane {
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
//
// One attempt, deliberately, where the reads retry. A failure here costs
// registry disk and a later garbage collection; a retry costs the backoff on
// every image of every build being destroyed, while an operator waits for a
// teardown. Clearing a schema of fifty builds against a registry that is
// down would spend minutes asking again for deletes whose whole contract is
// that they may fail.
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
