package cmgr

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unsetenv clears a variable for the test and restores it afterwards, which
// a bare os.Unsetenv would not: the value would be gone for every later test
// in the package.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

// captureLog is a Manager logger that keeps warnings and errors in buf.
func captureLog(buf *bytes.Buffer) *logger {
	return &logger{logger: log.New(buf, "", 0), logLevel: WARN}
}

func TestInitBuildPlane(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    string
		set      bool
		external bool
		wantErr  bool
	}{
		{name: "unset is local"},
		{name: "empty is local", value: "", set: true},
		{name: "local", value: "local", set: true},
		{name: "external", value: "external", set: true, external: true},
		{name: "anything else is refused", value: "remote", set: true, wantErr: true},
		{name: "case matters", value: "External", set: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetenv(t, BUILD_PLANE_ENV)
			if tc.set {
				t.Setenv(BUILD_PLANE_ENV, tc.value)
			}
			m := &Manager{log: newLogger(DISABLED)}
			err := m.initBuildPlane()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s=%q was accepted", BUILD_PLANE_ENV, tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("initBuildPlane: %s", err)
			}
			if m.externalBuildPlane != tc.external {
				t.Fatalf("externalBuildPlane = %v, want %v", m.externalBuildPlane, tc.external)
			}
			want := BuildPlaneLocal
			if tc.external {
				want = BuildPlaneExternal
			}
			if got := m.BuildPlane(); got != want {
				t.Fatalf("BuildPlane() = %q, want %q", got, want)
			}
		})
	}
}

// Everything that needs the challenge tree or the local daemon refuses with
// ErrExternalBuildPlane, without touching the database or docker: none of
// these Managers has either.
func TestExternalBuildPlaneRefusesBuildOperations(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED), externalBuildPlane: true}

	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrExternalBuildPlane) {
			t.Errorf("%s: got %v, want ErrExternalBuildPlane", what, err)
		}
	}

	cu := m.DetectChanges("")
	if len(cu.Errors) != 1 {
		t.Fatalf("DetectChanges returned %d errors, want 1: %v", len(cu.Errors), cu.Errors)
	}
	refused("DetectChanges", cu.Errors[0])
	if len(cu.Added)+len(cu.Updated)+len(cu.Refreshed)+len(cu.Stale)+len(cu.Removed)+len(cu.Unmodified) != 0 {
		t.Errorf("DetectChanges classified something on an external build plane: %+v", cu)
	}

	cu = m.UpdateWithOptions("", UpdateOptions{PruneOldImages: true})
	if len(cu.Errors) != 1 {
		t.Fatalf("UpdateWithOptions returned %d errors, want 1: %v", len(cu.Errors), cu.Errors)
	}
	refused("UpdateWithOptions", cu.Errors[0])
	if !m.updateMu.TryLock() {
		t.Fatal("UpdateWithOptions left updateMu held")
	}
	m.updateMu.Unlock()

	builds, err := m.Build("cmgr/examples/custom-socat", []int{1}, "flag{%s}")
	refused("Build", err)
	if builds != nil {
		t.Errorf("Build returned builds on an external build plane: %v", builds)
	}

	_, err = m.ListBasePins()
	refused("ListBasePins", err)
	_, err = m.RefreshBasePins()
	refused("RefreshBasePins", err)
}

func TestSetDirectoriesExternalIgnoresChallengeDir(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dir      string
		set      bool
		wantNote bool
	}{
		{name: "missing directory", dir: filepath.Join(t.TempDir(), "missing"), set: true, wantNote: true},
		{name: "unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetenv(t, DIR_ENV)
			if tc.set {
				t.Setenv(DIR_ENV, tc.dir)
			}
			artifacts := filepath.Join(t.TempDir(), "var", "lib", "cork", "artifacts")
			t.Setenv(ARTIFACT_DIR_ENV, artifacts)

			var logged bytes.Buffer
			m := &Manager{log: captureLog(&logged), externalBuildPlane: true}
			if err := m.setDirectories(); err != nil {
				t.Fatalf("setDirectories on an external build plane: %s", err)
			}
			if m.chalDir != "" {
				t.Fatalf("an external build plane resolved a challenge directory: %q", m.chalDir)
			}
			// The artifacts directory is still this daemon's: it serves the
			// bundles, whoever built them.
			if info, err := os.Stat(artifacts); err != nil || !info.IsDir() {
				t.Fatalf("artifacts directory was not created: %v", err)
			}
			if noted := strings.Contains(logged.String(), DIR_ENV+" is set but ignored"); noted != tc.wantNote {
				t.Fatalf("ignored-setting note logged = %v, want %v; log: %s", noted, tc.wantNote, logged.String())
			}
		})
	}
}

// Presence is what counts: an empty CMGR_DIR is the working directory on a
// local build plane and an empty CMGR_BASE_PINS fails one, so either, left
// in a unit file that now runs an external plane, is worth naming.
func TestNoteIgnoredSettingKeysOnPresence(t *testing.T) {
	var logged bytes.Buffer
	m := &Manager{log: captureLog(&logged), externalBuildPlane: true}

	unsetenv(t, DIR_ENV)
	m.noteIgnoredSetting(DIR_ENV)
	if logged.Len() != 0 {
		t.Fatalf("an unset variable was reported: %s", logged.String())
	}

	t.Setenv(DIR_ENV, "")
	m.noteIgnoredSetting(DIR_ENV)
	if !strings.Contains(logged.String(), DIR_ENV+" is set but ignored") {
		t.Fatalf("an empty value was not reported: %s", logged.String())
	}
}

func TestInitBasePinsExternalLoadsNothing(t *testing.T) {
	// A pin file that would fail a local start (malformed) is not even read.
	pins := filepath.Join(t.TempDir(), "base-pins.json")
	if err := os.WriteFile(pins, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BASE_PINS_ENV, pins)
	m := &Manager{log: newLogger(DISABLED), externalBuildPlane: true}
	if err := m.initBasePins(); err != nil {
		t.Fatalf("initBasePins read the pin file on an external build plane: %s", err)
	}
	if m.basePins != nil {
		t.Fatal("an external build plane loaded base pins")
	}
	if m.basePinsChecksum() != 0 {
		t.Fatal("pin fingerprint is not 0 without pins")
	}
}

// writeSelfSignedCerts puts a self-signed certificate and its key under a
// temporary directory, as the CA, the certificate and the key under the
// names given, and returns the directory.
func writeSelfSignedCerts(t *testing.T, caName, certName, keyName string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cmgr"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pemCert(der)
	return writeCertDir(t, map[string][]byte{caName: cert, certName: cert, keyName: pemKey(t, key)})
}

// pemCert and pemKey encode a certificate and its EC key the way certs.d
// holds them and the registry client (newRegistryHTTPClient) reads them.
func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// writeCertDir lays the files out under a temporary directory, as certs.d
// would, and returns it.
func writeCertDir(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// initDocker on an external build plane creates no client, still reads the
// settings the worker daemons need, requires a registry and the material to
// talk to it, and checks the workers' own material: refused when it cannot
// be loaded, said out loud when there is none.
func TestInitDockerExternalNeedsNoDaemon(t *testing.T) {
	// A socket nothing listens on: a local build plane would fail to ping it.
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "no-daemon.sock"))
	t.Setenv(PORTS_ENV, "20000-20029")
	t.Setenv(PURGE_AFTER_PUSH_ENV, "true")
	t.Setenv(REGISTRY_ENV, "zot.internal")
	t.Setenv(REGISTRY_CERT_DIR_ENV, writeSelfSignedCerts(t, "ca.crt", "client.cert", "client.key"))
	unsetenv(t, "DOCKER_CERT_PATH")
	unsetenv(t, IFACE_ENV)

	t.Run("everything in place", func(t *testing.T) {
		t.Setenv("DOCKER_CERT_PATH", writeSelfSignedCerts(t, "ca.pem", "cert.pem", "key.pem"))
		var logged bytes.Buffer
		m := &Manager{log: captureLog(&logged), externalBuildPlane: true}
		if err := m.initDocker(); err != nil {
			t.Fatalf("initDocker: %s", err)
		}
		if m.cli != nil {
			t.Fatal("an external build plane created a docker client")
		}
		if m.ctx == nil {
			t.Fatal("m.ctx is nil: the registry client and the worker control calls derive from it")
		}
		if m.hostOSType != "linux" {
			t.Fatalf("hostOSType = %q, want linux", m.hostOSType)
		}
		if m.purgeAfterPush {
			t.Fatal("purge-after-push is on with nothing to purge")
		}
		// The settings every daemon shares are read on the way through: a
		// nil queue here would be the first launch's panic, and an empty
		// interface the first port's.
		if m.portLow != 20000 || m.portHigh != 20029 {
			t.Fatalf("port range = %d-%d, want 20000-20029", m.portLow, m.portHigh)
		}
		if m.challengeInterface != "0.0.0.0" {
			t.Fatalf("challenge interface = %q", m.challengeInterface)
		}
		if m.challengeRegistry != "zot.internal" {
			t.Fatalf("registry = %q", m.challengeRegistry)
		}
		if m.localQueue == nil || m.launchConcurrency == 0 {
			t.Fatal("launch slots were not set up")
		}
		if strings.Contains(logged.String(), "DOCKER_CERT_PATH") {
			t.Fatalf("DOCKER_CERT_PATH was set, yet: %s", logged.String())
		}
	})

	t.Run("without the workers' material", func(t *testing.T) {
		var logged bytes.Buffer
		m := &Manager{log: captureLog(&logged), externalBuildPlane: true}
		if err := m.initDocker(); err != nil {
			t.Fatalf("initDocker: %s", err)
		}
		if !strings.Contains(logged.String(), "DOCKER_CERT_PATH is unset") {
			t.Fatalf("no warning about the missing worker material: %s", logged.String())
		}
	})

	t.Run("with workers' material it cannot load", func(t *testing.T) {
		t.Setenv("DOCKER_CERT_PATH", t.TempDir())
		m := &Manager{log: newLogger(DISABLED), externalBuildPlane: true}
		if err := m.initDocker(); err == nil {
			t.Fatal("an external build plane started with worker TLS material it cannot load")
		}
	})

	t.Run("without a registry", func(t *testing.T) {
		unsetenv(t, REGISTRY_ENV)
		m := &Manager{log: newLogger(DISABLED), externalBuildPlane: true}
		err := m.initDocker()
		if err == nil {
			t.Fatal("an external build plane started without a registry")
		}
		if !strings.Contains(err.Error(), REGISTRY_ENV) {
			t.Fatalf("error does not name %s: %s", REGISTRY_ENV, err)
		}
	})

	t.Run("without the registry client material", func(t *testing.T) {
		t.Setenv(REGISTRY_CERT_DIR_ENV, t.TempDir())
		m := &Manager{log: newLogger(DISABLED), externalBuildPlane: true}
		err := m.initDocker()
		if err == nil {
			t.Fatal("an external build plane started without a registry client, its only image cleanup")
		}
		if !strings.Contains(err.Error(), REGISTRY_CERT_DIR_ENV) {
			t.Fatalf("error does not name %s: %s", REGISTRY_CERT_DIR_ENV, err)
		}
	})
}

// setupExternalTestManager is setupTestManager on an external build plane:
// no docker client, which is the shape of the real thing rather than merely
// the shape of a test.
func setupExternalTestManager(t *testing.T) *Manager {
	t.Helper()
	m := setupTestManager(t)
	t.Cleanup(func() { m.db.Close() })
	m.externalBuildPlane = true
	m.ctx = context.Background()
	return m
}

// testChallenge is the retention fixture with a published port, so that it
// needs an instance and a launch of it reaches placement.
func testChallenge(id string, sourceChecksum uint32) *ChallengeMetadata {
	c := retentionTestChallenge(ChallengeId(id), sourceChecksum)
	c.PortMap = map[string]PortInfo{"web": {Host: "challenge", Port: 1337}}
	return c
}

// A schema converge on an external build plane neither scans a tree nor
// builds: a build that has been handed over passes, one that has not is
// named and refused, its row left as it was. The refusal an operator sees
// is requireHandedOver's, before the converge writes anything; this is
// the net under it.
func TestGenerateBuildsExternalRequiresIngestedBuilds(t *testing.T) {
	m := setupExternalTestManager(t)
	challenge := testChallenge("test/external-converge", 0)
	if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	ingested := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 1, 0x1111)
	pending := insertTestBuildRow(t, m, "", "event", string(challenge.Id), "flag{%s}", 2, 0x2222)

	lookup := func(id BuildId) *BuildMetadata {
		t.Helper()
		b, err := m.lookupBuildMetadata(id)
		if err != nil {
			t.Fatalf("lookupBuildMetadata(%d): %s", id, err)
		}
		return b
	}

	err := m.generateBuilds([]*BuildMetadata{lookup(ingested), lookup(pending)})
	if !errors.Is(err, ErrExternalBuildPlane) {
		t.Fatalf("a build nobody ingested was accepted: %v", err)
	}
	for _, want := range []string{string(challenge.Id), "'event'", "seed 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %s", want, err)
		}
	}
	if strings.Contains(err.Error(), "seed 1") {
		t.Errorf("error names the ingested build: %s", err)
	}
	if _, err := m.lookupBuildMetadata(pending); err != nil {
		t.Errorf("the refusal touched the row of the build nobody handed over: %s", err)
	}
	if _, err := m.lookupBuildMetadata(ingested); err != nil {
		t.Errorf("the ingested build lost its row: %s", err)
	}

	// Every build present: nothing to do, and no tree is consulted (there
	// is none to consult; m.chalDir is empty).
	var logged bytes.Buffer
	m.log = captureLog(&logged)
	if err := m.generateBuilds([]*BuildMetadata{lookup(ingested)}); err != nil {
		t.Fatalf("a converge over ingested builds failed: %s", err)
	}
	if strings.Contains(logged.String(), "earlier source generation") {
		t.Fatalf("a current build was called stale: %s", logged.String())
	}

	// A build produced from an earlier source generation is still said, as
	// a local converge would say it: the rebuild is the build plane's to
	// hand over.
	moved := testChallenge("test/external-moved-on", 0x3333)
	if errs := m.addChallenges([]*ChallengeMetadata{moved}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	stale := insertTestBuild(t, m, "event", string(moved.Id), "flag{%s}", 1, 0x1111)
	if err := m.generateBuilds([]*BuildMetadata{lookup(stale)}); err != nil {
		t.Fatalf("a converge over a stale build failed: %s", err)
	}
	if !strings.Contains(logged.String(), "earlier source generation") {
		t.Fatalf("a stale build went unremarked: %s", logged.String())
	}
}

// Destroying a build on an external build plane touches no local daemon (a
// nil client would panic) and still removes the row and the archive; the
// registry untag is attempted, best effort as ever.
func TestDestroyImagesExternalIsRegistryOnly(t *testing.T) {
	m := setupExternalTestManager(t)
	m.challengeRegistry = "zot.internal"
	m.artifactsDir = t.TempDir()
	// No certificates here: the registry untag fails, and is logged, not
	// returned -- the same contract as on a local build plane.
	t.Setenv(REGISTRY_CERT_DIR_ENV, t.TempDir())

	challenge := testChallenge("test/external-destroy", 0)
	if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	id := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 1, 0x1111)
	build := &BuildMetadata{
		Id:           id,
		Flag:         "flag{x}",
		Checksum:     0x1111,
		PrevChecksum: 0x1010,
		HasArtifacts: true,
		Images:       []Image{{Host: "challenge", Ports: []string{"1337/tcp"}}},
	}
	if err := m.finalizeBuild(build); err != nil {
		t.Fatalf("finalizeBuild: %s", err)
	}
	archive := filepath.Join(m.artifactsDir, build.getArtifactsFilename())
	if err := os.WriteFile(archive, []byte("gz"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.destroyImages(id); err != nil {
		t.Fatalf("destroyImages: %s", err)
	}
	if _, err := m.lookupBuildMetadata(id); err == nil {
		t.Error("the destroyed build kept its row")
	}
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the destroyed build's archive is still there: %v", err)
	}
}

// pruneReplacedImages is the same shape: no local daemon, registry only.
func TestPruneReplacedImagesExternalIsRegistryOnly(t *testing.T) {
	m := setupExternalTestManager(t)
	m.challengeRegistry = "zot.internal"
	t.Setenv(REGISTRY_CERT_DIR_ENV, t.TempDir())
	m.pruneReplacedImages([]replacedImages{{
		tags: []string{"zot.internal/test/prune:s1-1010-challenge"},
		meta: BuildMetadata{Challenge: "test/prune", Seed: 1, Format: "flag{%s}", Checksum: 0x1010},
	}})
}

// A launch with no worker to place it on is refused before anything is
// recorded: an external build plane has no local daemon to fall back to.
func TestNewInstanceExternalNeedsAWorker(t *testing.T) {
	for _, placement := range []bool{true, false} {
		t.Run(map[bool]string{true: "placement enabled", false: "placement disabled"}[placement], func(t *testing.T) {
			m := setupExternalTestManager(t)
			m.placementEnabled = placement
			m.workers = map[string]*workerConn{}
			challenge := testChallenge("test/external-launch", 0)
			if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
				t.Fatalf("addChallenges: %v", errs)
			}
			id := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 1, 0x1111)
			build, err := m.lookupBuildMetadata(id)
			if err != nil {
				t.Fatalf("lookupBuildMetadata: %s", err)
			}

			_, err = m.newInstance(build, nil, m.requestLimits())
			if !errors.Is(err, ErrNoWorkers) {
				t.Fatalf("got %v, want ErrNoWorkers", err)
			}
			var rows int
			if err := m.db.Get(&rows, "SELECT COUNT(1) FROM instances;"); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("%d instance row(s) recorded for a refused launch", rows)
			}
		})
	}
}

func TestInstanceClientExternalRefusesLocalInstances(t *testing.T) {
	local := &InstanceMetadata{Id: 7, Worker: ""}

	m := &Manager{log: newLogger(DISABLED), externalBuildPlane: true}
	if _, err := m.instanceClient(local); err == nil {
		t.Fatal("an external build plane handed out a client for a locally placed instance")
	}

	// Unchanged on a local build plane: the local client, whatever it is.
	m = &Manager{log: newLogger(DISABLED)}
	if cli, err := m.instanceClient(local); err != nil || cli != m.cli {
		t.Fatalf("local build plane: got (%v, %v)", cli, err)
	}
}

// A stop of an instance placed on the local daemon clears its records, the
// way a stop on a down worker does, so such a row can never wedge a schema
// delete or a prune on a daemon that cannot reach it.
func TestStopInstanceExternalClearsLocalRecords(t *testing.T) {
	m := setupExternalTestManager(t)
	challenge := testChallenge("test/external-stop", 0)
	if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	build := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 1, 0x1111)
	res, err := m.db.Exec("INSERT INTO instances(build, is_finalized, worker) VALUES (?, 1, '');", build)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	if err := m.stopInstance(&InstanceMetadata{Id: InstanceId(id), Build: build, Worker: ""}); err != nil {
		t.Fatalf("stopInstance: %s", err)
	}
	var rows int
	if err := m.db.Get(&rows, "SELECT COUNT(1) FROM instances WHERE id = ?;", id); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("the instance's records were not cleared")
	}
}

// The one startup check over what only a local daemon could serve: builds
// without a content checksum and instances placed on the local daemon,
// reported together, and only on an external build plane.
func TestCheckExternalBuildPlane(t *testing.T) {
	m := setupExternalTestManager(t)
	challenge := testChallenge("test/external-check", 0)
	if errs := m.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	current := insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 1, 0x1111)

	if err := m.checkExternalBuildPlane(); err != nil {
		t.Fatalf("refused a database it can serve: %s", err)
	}

	res, err := m.db.Exec("INSERT INTO instances(build, is_finalized, worker) VALUES (?, 1, '');", current)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := res.LastInsertId()
	err = m.checkExternalBuildPlane()
	if err == nil || !strings.Contains(err.Error(), "1 instance(s)") {
		t.Fatalf("a local instance was not refused, or not counted: %v", err)
	}
	if strings.Contains(err.Error(), "build(s)") {
		t.Fatalf("builds were named with none affected: %s", err)
	}

	insertTestBuild(t, m, "event", string(challenge.Id), "flag{%s}", 2, 0)
	err = m.checkExternalBuildPlane()
	if err == nil || !strings.Contains(err.Error(), "1 build(s)") || !strings.Contains(err.Error(), "1 instance(s)") {
		t.Fatalf("both problems are not reported in one message: %v", err)
	}
	if !strings.Contains(err.Error(), BUILD_PLANE_ENV+"="+BuildPlaneLocal) {
		t.Fatalf("the message does not name the way out: %s", err)
	}

	if _, err := m.db.Exec("UPDATE instances SET worker = '10.0.0.1' WHERE id = ?;", instance); err != nil {
		t.Fatal(err)
	}
	err = m.checkExternalBuildPlane()
	if err == nil || !strings.Contains(err.Error(), "1 build(s)") || strings.Contains(err.Error(), "instance(s)") {
		t.Fatalf("a legacy build alone is not reported alone: %v", err)
	}

	// A build whose challenge row is gone (foreign keys were off for an
	// out-of-band edit) is not counted: the migration a local start runs
	// skips it the same way, so counting it would send the operator to a
	// start that changes nothing. One connection, so the pragma and the
	// insert meet.
	m.db.SetMaxOpenConns(1)
	if _, err := m.db.Exec("PRAGMA foreign_keys = OFF;"); err != nil {
		t.Fatal(err)
	}
	insertTestBuild(t, m, "event", "test/gone", "flag{%s}", 1, 0)
	if _, err := m.db.Exec("PRAGMA foreign_keys = ON;"); err != nil {
		t.Fatal(err)
	}
	err = m.checkExternalBuildPlane()
	if err == nil || !strings.Contains(err.Error(), "1 build(s)") {
		t.Fatalf("an orphan build was counted, or the legacy one lost: %v", err)
	}

	// Never a concern on a local build plane, whatever the rows say.
	if _, err := m.db.Exec("UPDATE instances SET worker = '' WHERE id = ?;", instance); err != nil {
		t.Fatal(err)
	}
	m.externalBuildPlane = false
	if err := m.checkExternalBuildPlane(); err != nil {
		t.Fatalf("a local build plane refused its own rows: %s", err)
	}
}

// Builds that predate content-addressed tags need the local daemon to
// migrate (retag and push). An external build plane leaves them at 0 rather
// than stamping checksums whose tags exist nowhere, and refuses to start on
// them; the same database migrates as before on a local build plane.
func TestInitDatabaseExternalSkipsChecksumBackfill(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cmgr.db")
	t.Setenv(DB_ENV, dbPath)

	seed := &Manager{log: newLogger(DISABLED)}
	if err := seed.initDatabase(); err != nil {
		t.Fatalf("initDatabase: %s", err)
	}
	challenge := testChallenge("test/legacy", 0)
	if errs := seed.addChallenges([]*ChallengeMetadata{challenge}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	legacy := insertTestBuild(t, seed, "event", string(challenge.Id), "flag{%s}", 1, 0)
	seed.db.Close()

	external := &Manager{log: newLogger(DISABLED), externalBuildPlane: true}
	if err := external.initDatabase(); err != nil {
		t.Fatalf("initDatabase on an external build plane: %s", err)
	}
	defer external.db.Close()
	var checksum uint32
	if err := external.db.Get(&checksum, "SELECT checksum FROM builds WHERE id = ?;", legacy); err != nil {
		t.Fatal(err)
	}
	if checksum != 0 {
		t.Fatal("an external build plane stamped a checksum it could not retag for")
	}
	if err := external.checkExternalBuildPlane(); err == nil {
		t.Fatal("an external build plane accepted a database with unmigrated builds")
	}
	external.db.Close()

	local := &Manager{log: newLogger(DISABLED)}
	if err := local.initDatabase(); err != nil {
		t.Fatalf("a local build plane failed on the same database: %s", err)
	}
	defer local.db.Close()
	if err := local.db.Get(&checksum, "SELECT checksum FROM builds WHERE id = ?;", legacy); err != nil {
		t.Fatal(err)
	}
	if checksum == 0 {
		t.Fatal("the local build plane did not migrate the build")
	}
}
