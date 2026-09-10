package cmgr

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A staging file that a hand-over of this process still holds is never
// swept, however old it looks. Staging is over in a moment, but the
// hand-over then waits its turn at updateMu and commits every earlier build
// before this one is installed, and a sweep that took the file meanwhile
// would fail the hand-over on an archive it had already taken whole.
func TestSweepStagedArchivesSparesHeldOnes(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{log: newLogger(DISABLED), artifactsDir: dir}
	stale := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("archive"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * stagedArchiveMaxAge)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}
	abandoned := stale(stagedArchivePrefix + "abandoned.staged")
	held := stale(stagedArchivePrefix + "held.staged")
	served := stale("7.tar.gz")

	m.holdStagedArchive(held)
	m.sweepStagedArchives()

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("what a dead process staged was not reclaimed: %v", err)
	}
	if _, err := os.Stat(held); err != nil {
		t.Errorf("the staging file of a hand-over still in flight was swept: %v", err)
	}
	if _, err := os.Stat(served); err != nil {
		t.Errorf("an archive the daemon serves was swept: %v", err)
	}

	// Released with the hand-over that held it, it is an abandoned file like
	// any other.
	m.releaseStagedArchives(map[int]string{0: held})
	m.sweepStagedArchives()
	if _, err := os.Stat(held); !os.IsNotExist(err) {
		t.Errorf("a released staging file survived the next sweep: %v", err)
	}
}

// handOverFixture is a daemon on an external build plane with a fake
// registry to ask: `present` holds the manifest paths the registry serves,
// so a test decides per image tag what has been pushed.
type handOverFixture struct {
	m       *Manager
	present map[string]bool
	seen    *[]*http.Request
}

func setupHandOverFixture(t *testing.T) *handOverFixture {
	t.Helper()
	f := &handOverFixture{present: map[string]bool{}}
	host, seen := startFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && f.present[r.URL.Path] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	f.seen = seen
	f.m = setupExternalTestManager(t)
	f.m.challengeRegistry = host
	f.m.artifactsDir = t.TempDir()
	return f
}

// serve makes the registry answer for every image tag of the build.
func (f *handOverFixture) serve(challenge ChallengeId, build *BuildMetadata) {
	for _, image := range build.Images {
		f.present["/v2/"+string(challenge)+"/manifests/"+build.dockerId(image)] = true
	}
}

// handOver is a payload for the challenge as the build plane would send it:
// every build stamped with the identity its inputs give under this daemon's
// templates, and serves them all unless a test says otherwise.
func (f *handOverFixture) handOver(id ChallengeId, source, pins uint32, builds ...*BuildMetadata) *HandOver {
	challenge := testChallenge(string(id), source)
	challenge.MetadataChecksum = 0x10
	for _, build := range builds {
		build.Challenge = id
		build.SourceChecksum = source
		build.Checksum = contentChecksum(source, build.Format, pins, f.m.templateChecksum(challenge.ChallengeType))
		f.serve(id, build)
	}
	challenge.Builds = builds
	return &HandOver{Challenge: challenge, PinFingerprint: pins}
}

// delivered is one build as a hand-over carries it: on demand, so nothing
// is launched when it lands, with one image on the challenge host.
func delivered(seed int, flag string, artifacts bool) *BuildMetadata {
	return &BuildMetadata{
		Schema:        "event",
		Format:        "flag{%s}",
		Seed:          seed,
		Flag:          flag,
		LookupData:    map[string]string{"answer": flag + "!"},
		InstanceCount: DYNAMIC_INSTANCES,
		Images:        []Image{{Host: "challenge", Ports: []string{"1337/tcp"}}},
		HasArtifacts:  artifacts,
	}
}

func gzipTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// archivesOf serves the archives by build index, and counts what was asked.
func archivesOf(byIndex map[int][]byte, asked *[]int) ArchiveSource {
	return func(i int) (io.ReadCloser, error) {
		if asked != nil {
			*asked = append(*asked, i)
		}
		data, ok := byIndex[i]
		if !ok {
			return nil, fmt.Errorf("no archive for build %d", i)
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

func onlyBucket(t *testing.T, cu *ChallengeUpdates, want string) *ChallengeMetadata {
	t.Helper()
	buckets := map[string][]*ChallengeMetadata{
		"Added": cu.Added, "Updated": cu.Updated, "Refreshed": cu.Refreshed,
		"Stale": cu.Stale, "Unmodified": cu.Unmodified, "Removed": cu.Removed,
	}
	var found *ChallengeMetadata
	for name, bucket := range buckets {
		if len(bucket) == 0 {
			continue
		}
		if name != want || len(bucket) != 1 {
			t.Fatalf("verdict %s holds %d, want only one %s", name, len(bucket), want)
		}
		found = bucket[0]
	}
	if found == nil {
		t.Fatalf("no %s verdict: %+v", want, cu)
	}
	return found
}

// A hand-over of a challenge nobody has seen records the challenge and its
// builds as the build plane delivered them, asks the registry for every
// image tag first, and installs each archive under the row's own id.
func TestHandOverRecordsWhatTheBuildPlaneBuilt(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/handed-over")
	first := delivered(1, "flag{one}", true)
	second := delivered(2, "flag{two}", false)
	// The build plane's own ids and instances mean nothing here.
	first.Id, second.Id = 900, 901
	first.Instances = []*InstanceMetadata{{Id: 77}}
	ho := f.handOver(id, 0x1111, 0, first, second)

	asked := []int{}
	archives := archivesOf(map[int][]byte{0: gzipTar(t, map[string]string{"a.txt": "A", "dir/b.txt": "B"})}, &asked)
	cu, err := f.m.HandOverChallenge(id, ho, archives, UpdateOptions{})
	if err != nil {
		t.Fatalf("HandOverChallenge: %s", err)
	}
	if len(cu.Errors) > 0 {
		t.Fatalf("errors: %v", cu.Errors)
	}
	recorded := onlyBucket(t, cu, "Added")
	if len(recorded.Builds) != 2 {
		t.Fatalf("recorded %d builds, want 2", len(recorded.Builds))
	}
	if len(asked) != 1 || asked[0] != 0 {
		t.Errorf("archives asked for builds %v, want only the one that declares artifacts", asked)
	}
	if len(*f.seen) != 2 {
		t.Errorf("the registry saw %d requests, want one HEAD per image", len(*f.seen))
	}
	for _, r := range *f.seen {
		if r.Method != http.MethodHead {
			t.Errorf("registry saw %s, want HEAD", r.Method)
		}
	}

	for i, want := range []*BuildMetadata{first, second} {
		got := recorded.Builds[i]
		if got.Id == 0 || got.Id == want.Id {
			t.Errorf("build %d recorded under id %d, want this daemon's own", i, got.Id)
		}
		if got.Flag != want.Flag || got.Checksum != want.Checksum || got.SourceChecksum != 0x1111 || got.HasArtifacts != want.HasArtifacts {
			t.Errorf("build %d recorded as %+v", i, got)
		}
		if got.Schema != "event" || got.Format != "flag{%s}" || got.InstanceCount != DYNAMIC_INSTANCES || got.Seed != want.Seed {
			t.Errorf("build %d lost its schema, format, seed or instance count: %+v", i, got)
		}
		if got.LookupData["answer"] != want.Flag+"!" {
			t.Errorf("build %d lost its lookup data: %v", i, got.LookupData)
		}
		if len(got.Images) != 1 || got.Images[0].Host != "challenge" || len(got.Images[0].Ports) != 1 || got.Images[0].Ports[0] != "1337/tcp" {
			t.Errorf("build %d lost its images: %+v", i, got.Images)
		}
		if len(got.Instances) != 0 {
			t.Errorf("build %d got instances from the payload: %+v", i, got.Instances)
		}
		if got.PrevChecksum != 0 {
			t.Errorf("a first generation got rollback target %x", got.PrevChecksum)
		}
	}
	archive := filepath.Join(f.m.artifactsDir, recorded.Builds[0].getArtifactsFilename())
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("the archive was not installed under the row's id: %s", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "a.txt,dir/b.txt" && strings.Join(names, ",") != "dir/b.txt,a.txt" {
		t.Errorf("installed archive holds %v", names)
	}
	if _, err := os.Stat(filepath.Join(f.m.artifactsDir, recorded.Builds[1].getArtifactsFilename())); err == nil {
		t.Error("a build without artifacts got an archive")
	}
	if stored, err := f.m.lookupChallengeMetadata(id); err != nil || stored.SourceChecksum != 0x1111 {
		t.Errorf("challenge row: %+v, %v", stored, err)
	}
}

// Handed over again unchanged, the challenge is unmodified and the rows are
// the same; handed over at a new source generation, it is updated and each
// row rotates its retention pair; with its metadata alone changed, it is
// refreshed.
func TestHandOverVerdictsFollowTheChallenge(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/generations")
	deliver := func(source uint32) (*ChallengeUpdates, error) {
		ho := f.handOver(id, source, 0, delivered(1, "flag{"+fmt.Sprint(source)+"}", false))
		return f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{})
	}
	cu, err := deliver(0x1111)
	if err != nil {
		t.Fatal(err)
	}
	firstId := onlyBucket(t, cu, "Added").Builds[0].Id
	firstChecksum := onlyBucket(t, cu, "Added").Builds[0].Checksum

	cu, err = deliver(0x1111)
	if err != nil {
		t.Fatal(err)
	}
	again := onlyBucket(t, cu, "Unmodified").Builds[0]
	if again.Id != firstId || again.PrevChecksum != 0 {
		t.Errorf("the same generation again: %+v, want row %d with no rollback target", again, firstId)
	}

	cu, err = deliver(0x2222)
	if err != nil {
		t.Fatal(err)
	}
	next := onlyBucket(t, cu, "Updated").Builds[0]
	if next.Id != firstId {
		t.Errorf("a new generation opened a new row %d instead of rebuilding %d", next.Id, firstId)
	}
	if next.PrevChecksum != firstChecksum || next.Checksum == firstChecksum || next.SourceChecksum != 0x2222 {
		t.Errorf("the new generation did not rotate: %+v (first checksum %x)", next, firstChecksum)
	}
	if stored, _ := f.m.lookupChallengeMetadata(id); stored.SourceChecksum != 0x2222 {
		t.Errorf("challenge row still at source generation %x", stored.SourceChecksum)
	}

	ho := f.handOver(id, 0x2222, 0, delivered(1, "flag{8738}", false))
	ho.Challenge.MetadataChecksum = 0x11
	ho.Challenge.Description = "reworded"
	cu, err = f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	onlyBucket(t, cu, "Refreshed")
	if stored, _ := f.m.lookupChallengeMetadata(id); stored.MetadataChecksum != 0x11 || stored.Description != "reworded" {
		t.Errorf("a refresh did not persist the metadata: %+v", stored)
	}
}

// Refusals write nothing: the local build plane's, a payload that
// contradicts itself or its inputs, and a tag the registry does not serve.
func TestHandOverRefusals(t *testing.T) {
	local := setupTestManager(t)
	defer local.db.Close()
	if _, err := local.HandOverChallenge("test/x", &HandOver{}, archivesOf(nil, nil), UpdateOptions{}); !errors.Is(err, ErrLocalBuildPlane) {
		t.Errorf("a local build plane took a hand-over: %v", err)
	}

	f := setupHandOverFixture(t)
	id := ChallengeId("test/refused")
	cases := []struct {
		name  string
		tweak func(ho *HandOver)
		want  error
		says  string
	}{
		{"the wrong challenge", func(ho *HandOver) { ho.Challenge.Id = "test/other" }, ErrHandOverInvalid, "describes 'test/other'"},
		{"a forged content checksum", func(ho *HandOver) { ho.Challenge.Builds[0].Checksum++ }, ErrHandOverInvalid, "content checksum"},
		{"a build of another source generation", func(ho *HandOver) { ho.Challenge.Builds[0].SourceChecksum = 0x9999 }, ErrHandOverInvalid, "source generation"},
		{"a build without a flag", func(ho *HandOver) { ho.Challenge.Builds[0].Flag = "" }, ErrHandOverInvalid, "no flag"},
		{"a null build", func(ho *HandOver) { ho.Challenge.Builds[0] = nil }, ErrHandOverInvalid, "is null"},
		{"a locked build", func(ho *HandOver) { ho.Challenge.Builds[0].InstanceCount = LOCKED }, ErrHandOverInvalid, "instance count"},
		{"the same build twice", func(ho *HandOver) { ho.Challenge.Builds = append(ho.Challenge.Builds, ho.Challenge.Builds[0]) }, ErrHandOverInvalid, "twice"},
		{"an image for a host the challenge lacks", func(ho *HandOver) { ho.Challenge.Builds[0].Images[0].Host = "ghost" }, ErrHandOverInvalid, "host 'ghost'"},
		// The other way round: a host the build has no image for. The launch
		// would start the hosts it has and call the instance finalized.
		{"a host with no image", func(ho *HandOver) {
			ho.Challenge.Hosts = append(ho.Challenge.Hosts, HostInfo{Name: "sidecar"})
		}, ErrHandOverInvalid, "no image for host 'sidecar'"},
		{"a tag the registry does not serve", func(ho *HandOver) { ho.Challenge.Builds[0].Seed = 99 }, ErrNotInRegistry, "s99-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))
			tc.tweak(ho)
			_, err := f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal does not say %q: %s", tc.says, err)
			}
			if _, err := f.m.lookupChallengeMetadata(id); err == nil {
				t.Error("the refused hand-over left a challenge row")
			}
		})
	}
	if _, err := f.m.HandOverChallenge(id, nil, archivesOf(nil, nil), UpdateOptions{}); !errors.Is(err, ErrHandOverInvalid) {
		t.Errorf("a hand-over with no payload at all: %v", err)
	}

	// A refusal for what the payload says never reaches the registry; one
	// for what the registry says does.
	heads := 0
	for _, r := range *f.seen {
		if r.Method == http.MethodHead {
			heads++
		}
	}
	if heads != 1 {
		t.Errorf("the registry saw %d HEADs across the refusals, want the one lookup of the unserved tag", heads)
	}

	// The build plane's own builder image is never in the registry and is
	// not asked for.
	ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))
	ho.Challenge.Hosts = append(ho.Challenge.Hosts, HostInfo{Name: "builder"})
	ho.Challenge.Builds[0].Images = append(ho.Challenge.Builds[0].Images, Image{Host: "builder", Ports: []string{}})
	if cu, err := f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{}); err != nil || len(cu.Errors) > 0 {
		t.Fatalf("a build with a builder image was refused: %v %v", err, cu)
	}
}

// An archive that cannot be taken refuses the whole hand-over, before
// anything is written: the archives are read in one pass ahead of the rows,
// so a bad one leaves no half-recorded challenge to explain, and nothing
// staged behind either.
func TestHandOverRefusesAnUnreadableArchive(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/partial")
	empty := func() {
		t.Helper()
		entries, err := os.ReadDir(f.m.artifactsDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			names := []string{}
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("the refused hand-over left %v in the artifacts directory", names)
		}
		if _, err := f.m.lookupChallengeMetadata(id); err == nil {
			t.Fatal("the refused hand-over left a challenge row")
		}
	}

	// The sound archive of the first build is staged before the second is
	// found to be unreadable, and goes with the refusal.
	ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", true), delivered(2, "flag{two}", true))
	archives := archivesOf(map[int][]byte{0: gzipTar(t, map[string]string{"a.txt": "A"}), 1: []byte("not a gzip stream")}, nil)
	_, err := f.m.HandOverChallenge(id, ho, archives, UpdateOptions{})
	if !errors.Is(err, ErrHandOverInvalid) {
		t.Fatalf("an unreadable archive: %v, want the hand-over refused", err)
	}
	if !strings.Contains(err.Error(), "seed 2") {
		t.Errorf("the refusal does not name the build whose archive failed: %s", err)
	}
	empty()

	// A build that declares artifacts and brings none.
	ho = f.handOver(id, 0x1111, 0, delivered(3, "flag{three}", true))
	_, err = f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{})
	if !errors.Is(err, ErrHandOverInvalid) || !strings.Contains(err.Error(), "no archive for build 0") {
		t.Fatalf("a build whose archive was not sent: %v", err)
	}
	empty()

	// An archive with nothing in it, which would leave a download to serve
	// a bundle with no files.
	ho = f.handOver(id, 0x1111, 0, delivered(4, "flag{four}", true))
	_, err = f.m.HandOverChallenge(id, ho, archivesOf(map[int][]byte{0: gzipTar(t, nil)}, nil), UpdateOptions{})
	if !errors.Is(err, ErrHandOverInvalid) || !strings.Contains(err.Error(), "holds no files") {
		t.Fatalf("an empty archive: %v", err)
	}
	empty()
}

// The same hand-over twice changes nothing: the row is left as it is, and
// so is what runs it. A new generation is the rebuild it is, and takes the
// on-demand instances of the generation it replaces down.
func TestHandOverRepeatedLeavesWhatRunsItAlone(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/idempotent")
	deliver := func(source uint32, flag string) *ChallengeUpdates {
		t.Helper()
		cu, err := f.m.HandOverChallenge(id, f.handOver(id, source, 0, delivered(1, flag, false)), archivesOf(nil, nil), UpdateOptions{})
		if err != nil {
			t.Fatalf("HandOverChallenge: %s", err)
		}
		if len(cu.Errors) > 0 {
			t.Fatalf("errors: %v", cu.Errors)
		}
		return cu
	}
	first := onlyBucket(t, deliver(0x1111, "flag{one}"), "Added").Builds[0]

	// An on-demand instance of it, as a player's launch leaves one (the
	// columns openInstance writes). On this plane a stop clears the records
	// without docker, so a reconcile that ran here would take the row with
	// it.
	if _, err := f.m.db.Exec("INSERT INTO instances(build, lastsolved, is_finalized, worker) VALUES (?, 0, 1, '');", first.Id); err != nil {
		t.Fatal(err)
	}
	running := func() int {
		t.Helper()
		instances, err := f.m.getBuildInstances(first.Id)
		if err != nil {
			t.Fatal(err)
		}
		return len(instances)
	}

	again := onlyBucket(t, deliver(0x1111, "flag{one}"), "Unmodified").Builds[0]
	if again.Id != first.Id {
		t.Errorf("the same hand-over again moved the build to row %d", again.Id)
	}
	if running() != 1 {
		t.Error("the same hand-over again stopped the instance it found running")
	}

	next := onlyBucket(t, deliver(0x2222, "flag{two}"), "Updated").Builds[0]
	if next.PrevChecksum != first.Checksum {
		t.Errorf("a new generation did not rotate: %+v", next)
	}
	if running() != 0 {
		t.Error("a new generation left the instance of the one it replaced running")
	}
}

// A challenge that needs no instances is brought to none even where the
// build handed over is one the daemon already holds: an instance row under
// an artifact-only build is a placeholder an older cmgr left, and a
// converge is what clears it.
func TestHandOverRepeatedClearsPlaceholderInstances(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/artifact-only")
	deliver := func() *ChallengeUpdates {
		t.Helper()
		ho := f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))
		// No published port: the challenge is delivered by its artifacts
		// and nothing is ever launched for it.
		ho.Challenge.PortMap = nil
		ho.Challenge.Builds[0].Images[0].Ports = nil
		cu, err := f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{})
		if err != nil {
			t.Fatalf("HandOverChallenge: %s", err)
		}
		if len(cu.Errors) > 0 {
			t.Fatalf("errors: %v", cu.Errors)
		}
		return cu
	}
	build := onlyBucket(t, deliver(), "Added").Builds[0]
	if _, err := f.m.db.Exec("INSERT INTO instances(build, lastsolved, is_finalized, worker) VALUES (?, 0, 1, '');", build.Id); err != nil {
		t.Fatal(err)
	}

	onlyBucket(t, deliver(), "Unmodified")
	instances, err := f.m.getBuildInstances(build.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 0 {
		t.Error("a placeholder instance under a build that needs none survived a hand-over of what the daemon already held")
	}
}

// A persistent build is launched as it lands, as a schema converge
// launches one. With no worker registered that fails, and the failure is
// that build's: reported, with the build recorded all the same.
func TestHandOverLaunchesAPersistentBuild(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/persistent")
	build := delivered(1, "flag{one}", false)
	build.InstanceCount = 1
	cu, err := f.m.HandOverChallenge(id, f.handOver(id, 0x1111, 0, build), archivesOf(nil, nil), UpdateOptions{})
	if err != nil {
		t.Fatalf("HandOverChallenge: %s", err)
	}
	if len(cu.Errors) != 1 || !errors.Is(cu.Errors[0], ErrNoWorkers) {
		t.Fatalf("errors: %v, want the launch refused for want of a worker", cu.Errors)
	}
	recorded := onlyBucket(t, cu, "Added")
	if len(recorded.Builds) != 1 || recorded.Builds[0].Flag != "flag{one}" || recorded.Builds[0].InstanceCount != 1 {
		t.Fatalf("the build a launch failed for was not recorded: %+v", recorded.Builds)
	}
}

// A challenge with no builds on record can be removed; one with builds is
// refused until they go with their schema.
func TestRemoveChallenge(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/removable")
	var unknown *UnknownIdentifierError
	if err := f.m.RemoveChallenge(id); !errors.As(err, &unknown) {
		t.Fatalf("removing a challenge nobody handed over: %v", err)
	}

	// Metadata alone is a hand-over too.
	ho := f.handOver(id, 0x1111, 0)
	if cu, err := f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{}); err != nil || len(cu.Errors) > 0 {
		t.Fatalf("a hand-over without builds: %v %v", err, cu)
	}
	if err := f.m.RemoveChallenge(id); err != nil {
		t.Fatalf("RemoveChallenge: %s", err)
	}
	if _, err := f.m.lookupChallengeMetadata(id); !errors.As(err, &unknown) {
		t.Errorf("the challenge is still on record: %v", err)
	}

	ho = f.handOver(id, 0x1111, 0, delivered(1, "flag{one}", false))
	if cu, err := f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{}); err != nil || len(cu.Errors) > 0 {
		t.Fatalf("hand-over: %v %v", err, cu)
	}
	if err := f.m.RemoveChallenge(id); !errors.Is(err, ErrChallengeHasBuilds) {
		t.Fatalf("removing a challenge with a build: %v", err)
	}
	if _, err := f.m.lookupChallengeMetadata(id); err != nil {
		t.Errorf("the refused removal took the challenge: %v", err)
	}
}

// A multi-host challenge is handed over whole, one image per host. That is
// what the local build path makes by construction -- executeBuild walks
// cMeta.Hosts and appends an image for every one of them -- and what
// checkHandOver requires of a payload it cannot see built. The refusal is
// covered in TestHandOverRefusals; this is the other side of it, because a
// completeness check that turned a legitimate build away would be a worse
// fault than the gap it closes, and the e2e's challenges are single-host.
func TestHandOverTakesAMultiHostChallenge(t *testing.T) {
	f := setupHandOverFixture(t)
	id := ChallengeId("test/multi-host")
	build := delivered(1, "flag{one}", false)
	build.Images = append(build.Images, Image{Host: "sidecar", Ports: []string{"9000/tcp"}})
	// Served for both images, since handOver stamps the identity first.
	ho := f.handOver(id, 0x1111, 0, build)
	ho.Challenge.Hosts = append(ho.Challenge.Hosts, HostInfo{Name: "sidecar"})

	cu, err := f.m.HandOverChallenge(id, ho, archivesOf(nil, nil), UpdateOptions{})
	if err != nil {
		t.Fatalf("a challenge with an image for every host was refused: %s", err)
	}
	if len(cu.Errors) > 0 {
		t.Fatalf("errors: %v", cu.Errors)
	}
	recorded := onlyBucket(t, cu, "Added")
	if len(recorded.Builds) != 1 {
		t.Fatalf("recorded %d builds, want 1", len(recorded.Builds))
	}
	hosts := map[string]bool{}
	for _, image := range recorded.Builds[0].Images {
		hosts[image.Host] = true
	}
	if !hosts["challenge"] || !hosts["sidecar"] || len(hosts) != 2 {
		t.Errorf("the build was recorded with images for %v, want one per host", hosts)
	}
	// One HEAD per image, both hosts asked after.
	if len(*f.seen) != 2 {
		t.Errorf("the registry saw %d requests, want one HEAD per image", len(*f.seen))
	}
}
