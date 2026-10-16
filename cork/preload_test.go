package cork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
)

const preloadTestRegistry = "registry.test:5000"

func TestImageRepo(t *testing.T) {
	for name, want := range map[string]string{
		"registry.test:5000/test/web:s1-1111-challenge": "registry.test:5000/test/web",
		"registry.test:5000/test/web":                   "registry.test:5000/test/web",
		"test/web:s1-1111-challenge":                    "test/web",
		"web":                                           "web",
	} {
		if got := imageRepo(name); got != want {
			t.Errorf("imageRepo(%q) = %q, want %q", name, got, want)
		}
	}
}

// What a pass pulls and removes. A tag of a preloaded challenge that an
// on-demand schema still launches stays: what any current build launches is
// kept, not only what preloaded schemas want.
func TestPlanPreload(t *testing.T) {
	sets := &preloadSets{
		wanted: map[string]bool{"r/web:new": true, "r/web:held": true},
		named:  map[string]bool{"r/web:new": true, "r/web:held": true, "r/web:ondemand": true, "r/other:current": true},
		repos:  map[string]bool{"r/web": true},
	}
	held := heldTags([]image.Summary{
		{RepoTags: []string{"r/web:held"}},
		{RepoTags: []string{"r/web:old", "r/web:older"}},
		{RepoTags: []string{"r/web:ondemand"}},
		{RepoTags: []string{"r/other:current", "r/other:old"}}, // not preloaded: never removed
	})
	if got, want := missing(sets.wanted, held), []string{"r/web:new"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pulls = %v, want %v", got, want)
	}
	// The old versions wait for a new one the pass has yet to try, whose
	// layers they share, but not for one whose pull failed.
	if got := superseded(sets, held, nil); len(got) > 0 {
		t.Errorf("removals before the new image is tried = %v, want none", got)
	}
	want := []string{"r/web:old", "r/web:older"}
	if got := superseded(sets, held, map[string]bool{"r/web:new": true}); !reflect.DeepEqual(got, want) {
		t.Errorf("removals after the new image failed to pull = %v, want %v", got, want)
	}
	held["r/web:new"] = true
	if got := superseded(sets, held, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("removals = %v, want %v", got, want)
	}
}

// Released is what preloading wanted that it neither wants nor removes now.
func TestReleasedImages(t *testing.T) {
	before := &preloadSets{wanted: map[string]bool{
		"r/web:shared": true, "r/web:dropped": true, "r/pwn:launched": true, "r/gone:old": true,
	}}
	after := &preloadSets{
		wanted: map[string]bool{"r/web:shared": true},                         // another preloaded schema's
		named:  map[string]bool{"r/web:shared": true, "r/pwn:launched": true}, // r/web:dropped is removed
		repos:  map[string]bool{"r/web": true},
	}
	if got, want := releasedImages(before, after), []string{"r/gone:old", "r/pwn:launched"}; !reflect.DeepEqual(got, want) {
		t.Errorf("released = %v, want %v", got, want)
	}
}

// preloadFixture is an external orchestrator with three challenges: test/web
// (a service, preloaded through "event"), test/docs (artifact-only, also in
// "event") and test/other (a service in "archive").
type preloadFixture struct {
	m              *Manager
	web, other     BuildId
	webName        string // the image event launches
	otherName      string
	unbuiltWebSeed int
}

func newPreloadFixture(t *testing.T) *preloadFixture {
	t.Helper()
	m := setupExternalTestManager(t)
	m.challengeRegistry = preloadTestRegistry
	f := &preloadFixture{m: m, unbuiltWebSeed: 2}

	docs := retentionTestChallenge("test/docs", 0)
	if errs := m.addChallenges([]*ChallengeMetadata{testChallenge("test/web", 0), testChallenge("test/other", 0), docs}); len(errs) > 0 {
		t.Fatalf("addChallenges: %v", errs)
	}
	build := func(schema, challenge string, seed int, checksum uint32, hosts ...string) *BuildMetadata {
		t.Helper()
		b := &BuildMetadata{
			Id: insertTestBuild(t, m, schema, challenge, "flag{%s}", seed, checksum), Flag: "flag{x}",
			Challenge: ChallengeId(challenge), Seed: seed, Checksum: checksum,
		}
		for _, host := range hosts {
			b.Images = append(b.Images, Image{Host: host, Ports: []string{}})
		}
		if err := m.finalizeBuild(b); err != nil {
			t.Fatalf("finalizeBuild: %s", err)
		}
		return b
	}
	web := build("event", "test/web", 1, 0x1111, "challenge", "builder")
	build("event", "test/docs", 1, 0x3333, "challenge")
	other := build("archive", "test/other", 1, 0x2222, "challenge")
	insertTestBuildRow(t, m, "", "event", "test/web", "flag{%s}", f.unbuiltWebSeed, 0)

	f.web, f.other = web.Id, other.Id
	f.webName = m.instanceImageName(web.Challenge, web, Image{Host: "challenge"})
	f.otherName = m.instanceImageName(other.Challenge, other, Image{Host: "challenge"})
	if err := m.AddPreload("event"); err != nil {
		t.Fatalf("AddPreload: %s", err)
	}
	return f
}

func TestPreloadSets(t *testing.T) {
	f := newPreloadFixture(t)
	sets, err := f.m.preloadSets()
	if err != nil {
		t.Fatalf("preloadSets: %s", err)
	}
	// Only the built service image: not the builder host, not the
	// artifact-only challenge, not the unbuilt seed, not another schema.
	if want := map[string]bool{f.webName: true}; !reflect.DeepEqual(sets.wanted, want) {
		t.Errorf("wanted = %v, want %v", sets.wanted, want)
	}
	if !sets.named[f.webName] || !sets.named[f.otherName] {
		t.Errorf("named = %v, want both service images", sets.named)
	}
	// Cleanup covers the challenges whose images the workers hold.
	if want := map[string]bool{preloadTestRegistry + "/test/web": true}; !reflect.DeepEqual(sets.repos, want) {
		t.Errorf("repos = %v, want %v", sets.repos, want)
	}
	if want := []string{f.webName}; !reflect.DeepEqual(sets.bySchema["event"], want) {
		t.Errorf("bySchema[event] = %v, want %v", sets.bySchema["event"], want)
	}
}

func TestAddAndRemovePreload(t *testing.T) {
	f := newPreloadFixture(t)
	m := f.m

	if err := m.AddPreload("event"); err != nil {
		t.Errorf("a second AddPreload: %s", err)
	}
	var unknown *UnknownIdentifierError
	if err := m.AddPreload("nonesuch"); !errors.As(err, &unknown) {
		t.Errorf("AddPreload of an unknown schema = %v, want an unknown identifier", err)
	}
	if _, err := m.RemovePreload("event"); err != nil {
		t.Fatalf("RemovePreload: %s", err)
	}
	if _, err := m.RemovePreload("event"); !errors.As(err, &unknown) {
		t.Errorf("a second RemovePreload = %v, want an unknown identifier", err)
	}

	m.challengeRegistry = ""
	if err := m.AddPreload("event"); !errors.Is(err, ErrPreloadNeedsRegistry) {
		t.Errorf("AddPreload without a registry = %v, want ErrPreloadNeedsRegistry", err)
	}
}

// Deleting a preloaded schema leaves what it pulled on the workers, for an
// operator to vet, and says which images those are. Another schema stays
// preloaded, so the pass after the delete does run.
func TestDeletingAPreloadedSchemaLeavesItsImages(t *testing.T) {
	f := newPreloadFixture(t)
	if err := f.m.AddPreload("archive"); err != nil {
		t.Fatalf("AddPreload: %s", err)
	}
	var logged bytes.Buffer
	f.m.log = captureLog(&logged)
	old := preloadTestRegistry + "/test/web:s1-aaaa-challenge"
	d := newFakeImageDaemon(t,
		image.Summary{ID: "web", RepoTags: []string{f.webName}},
		image.Summary{ID: "old-web", RepoTags: []string{old}},
		image.Summary{ID: "other", RepoTags: []string{f.otherName}},
	)
	w := d.conn(t, f.m)
	if err := f.m.DeleteSchema("event", false); err != nil {
		t.Fatalf("DeleteSchema: %s", err)
	}
	var schemas []string
	if err := f.m.db.Select(&schemas, "SELECT schema FROM preloads;"); err != nil || !reflect.DeepEqual(schemas, []string{"archive"}) {
		t.Errorf("preloads after the delete = %v (err %v), want only archive", schemas, err)
	}
	if !strings.Contains(logged.String(), "no longer covers 1 image(s)") || !strings.Contains(logged.String(), f.webName) {
		t.Errorf("the delete did not log what preloading left behind:\n%s", logged.String())
	}
	f.m.preloadPass(w)
	if pulls, removes := d.calls(); len(pulls)+len(removes) > 0 {
		t.Errorf("after the delete, a pass pulled %v and removed %v", pulls, removes)
	}
	if w.preload.lastPass.IsZero() {
		t.Error("the pass after the delete did not run")
	}
}

// preload-remove returns what the schema leaves to on-demand cleanup: what
// another preloaded schema wants stays covered.
func TestRemovePreloadReportsWhatItReleases(t *testing.T) {
	f := newPreloadFixture(t)
	second := &BuildMetadata{
		Id: insertTestBuild(t, f.m, "second", "test/web", "flag{%s}", 9, 0x4444), Flag: "flag{x}",
		Challenge: "test/web", Seed: 9, Checksum: 0x4444, Images: []Image{{Host: "challenge", Ports: []string{}}},
	}
	if err := f.m.finalizeBuild(second); err != nil {
		t.Fatalf("finalizeBuild: %s", err)
	}
	secondName := f.m.instanceImageName(second.Challenge, second, Image{Host: "challenge"})
	shared := &BuildMetadata{ // event's web image, wanted by a third schema too
		Id: insertTestBuild(t, f.m, "shared", "test/web", "flag{%s}", 1, 0x1111), Flag: "flag{x}",
		Challenge: "test/web", Seed: 1, Checksum: 0x1111, Images: []Image{{Host: "challenge", Ports: []string{}}},
	}
	if err := f.m.finalizeBuild(shared); err != nil {
		t.Fatalf("finalizeBuild: %s", err)
	}
	for _, s := range []string{"second", "shared"} {
		if err := f.m.AddPreload(s); err != nil {
			t.Fatalf("AddPreload(%s): %s", s, err)
		}
	}
	for _, tc := range []struct {
		schema string
		want   []string
	}{
		{"event", []string{}},
		{"shared", []string{f.webName}},
		{"second", []string{secondName}},
	} {
		got, err := f.m.RemovePreload(tc.schema)
		if err != nil {
			t.Fatalf("RemovePreload(%s): %s", tc.schema, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("RemovePreload(%s) released %v, want %v", tc.schema, got, tc.want)
		}
	}
}

// fakeImageDaemon serves the image calls a preload pass makes, keeping the
// store they act on.
type fakeImageDaemon struct {
	srv       *httptest.Server
	mu        sync.Mutex
	images    []image.Summary
	pulls     []string
	removes   []string        // "<ref> force=<0|1>"
	tagged    []string        // "<source> <target>"
	onDelete  func()          // runs once a delete has untagged, before it answers, outside the lock
	onPull    func()          // runs as a pull arrives, outside the lock
	onList    func()          // runs as an image list arrives, outside the lock
	requests  []string        // every request, "<method> <path>"
	inUse     map[string]bool // image ids a container uses
	pullFails bool            // answer pulls with a stream error, as a registry missing the tag does
	listFails bool            // answer image lists with a 500
}

func newFakeImageDaemon(t *testing.T, images ...image.Summary) *fakeImageDaemon {
	t.Helper()
	d := &fakeImageDaemon{images: images, inUse: map[string]bool{}}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		delHook, pullHook, listHook := d.onDelete, d.onPull, d.onList
		d.mu.Unlock()
		if r.Method == "DELETE" {
			status, body := d.delete(r)
			// As dockerd: the tag is gone at once, and the delete's work
			// takes its time after it.
			if status == http.StatusOK && delHook != nil {
				delHook()
			}
			if status != http.StatusOK {
				http.Error(w, body.(string), status)
				return
			}
			json.NewEncoder(w).Encode(body)
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/images/create") && pullHook != nil {
			pullHook()
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/images/json") && listHook != nil {
			listHook()
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.requests = append(d.requests, r.Method+" "+r.URL.Path)
		_, ref, _ := strings.Cut(r.URL.Path, "/images/")
		switch {
		// What admitting a worker asks of it (runWorker), for an empty one.
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.44")
			w.Header().Set("Ostype", "linux")
		case strings.HasSuffix(r.URL.Path, "/containers/json"), strings.HasSuffix(r.URL.Path, "/networks"):
			io.WriteString(w, "[]")
		case r.Method == "GET" && ref == "json" && d.listFails:
			http.Error(w, "the daemon is unwell", http.StatusInternalServerError)
		case r.Method == "GET" && ref == "json":
			json.NewEncoder(w).Encode(d.images)
		case r.Method == "POST" && strings.HasSuffix(ref, "/tag"):
			source := strings.TrimSuffix(ref, "/tag")
			target := r.URL.Query().Get("repo") + ":" + r.URL.Query().Get("tag")
			i := slices.IndexFunc(d.images, func(img image.Summary) bool {
				return img.ID == source || slices.Contains(img.RepoTags, source)
			})
			if i < 0 {
				http.Error(w, "No such image: "+source, http.StatusNotFound)
				return
			}
			// As dockerd: a target held elsewhere moves, leaving that image
			// untagged rather than gone; one already here stays one.
			for j := range d.images {
				d.images[j].RepoTags = slices.DeleteFunc(slices.Clone(d.images[j].RepoTags), func(t string) bool { return t == target })
			}
			d.images[i].RepoTags = append(d.images[i].RepoTags, target)
			d.tagged = append(d.tagged, source+" "+target)
			w.WriteHeader(http.StatusCreated)
		case r.Method == "GET" && strings.HasSuffix(ref, "/json"):
			name := strings.TrimSuffix(ref, "/json")
			for _, img := range d.images {
				if img.ID == name || slices.Contains(img.RepoTags, name) {
					json.NewEncoder(w).Encode(image.InspectResponse{ID: img.ID})
					return
				}
			}
			http.Error(w, "No such image: "+name, http.StatusNotFound)
		case r.Method == "POST" && ref == "create" && d.pullFails:
			io.WriteString(w, `{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}`+"\n")
		case r.Method == "POST" && ref == "create":
			name := r.URL.Query().Get("fromImage") + ":" + r.URL.Query().Get("tag")
			d.pulls = append(d.pulls, name)
			d.images = append(d.images, image.Summary{ID: "pulled-" + name, RepoTags: []string{name}})
			io.WriteString(w, `{"status":"Pull complete"}`+"\n")
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// delete serves an image delete, answering a status and either the deletes'
// records or an error message.
func (d *fakeImageDaemon) delete(r *http.Request) (int, any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, r.Method+" "+r.URL.Path)
	_, ref, _ := strings.Cut(r.URL.Path, "/images/")
	force := "0"
	if r.URL.Query().Get("force") == "1" {
		force = "1"
	}
	d.removes = append(d.removes, ref+" force="+force)
	i := slices.IndexFunc(d.images, func(img image.Summary) bool {
		return img.ID == ref || slices.Contains(img.RepoTags, ref)
	})
	if i < 0 {
		return http.StatusNotFound, "No such image: " + ref
	}
	img := d.images[i]
	// As dockerd: unforced, the last tag of an image a container uses is
	// refused; one of several is only taken off.
	if force == "0" && d.inUse[img.ID] && len(img.RepoTags) <= 1 {
		return http.StatusConflict, "conflict: unable to remove repository reference " + ref + " (must force) - container is using its referenced image"
	}
	d.images[i].RepoTags = slices.DeleteFunc(slices.Clone(img.RepoTags), func(t string) bool { return t == ref })
	records := []image.DeleteResponse{{Untagged: ref}}
	if img.ID == ref || len(d.images[i].RepoTags) == 0 {
		d.images = slices.Delete(d.images, i, i+1)
		records = append(records, image.DeleteResponse{Deleted: img.ID})
	}
	return http.StatusOK, records
}

// tags is what a pass would have listed: every tag the daemon holds now.
func (d *fakeImageDaemon) tags() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return heldTags(d.images)
}

func (d *fakeImageDaemon) calls() (pulls, removes []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.pulls...), append([]string(nil), d.removes...)
}

func (d *fakeImageDaemon) conn(t *testing.T, m *Manager) *workerConn {
	t.Helper()
	cli, err := client.NewClientWithOpts(
		client.WithHost(strings.Replace(d.srv.URL, "http://", "tcp://", 1)),
		client.WithVersion("1.44"),
	)
	if err != nil {
		t.Fatalf("building a client for the fake daemon: %s", err)
	}
	w := &workerConn{
		ip: "127.0.0.1", cli: cli, queue: newDaemonQueue(1),
		unreachCh: make(chan struct{}), done: make(chan struct{}),
		probeNow: make(chan struct{}, 1), preloadNow: make(chan struct{}, 1),
	}
	w.reachable.Store(int32(workerReachableOk))
	w.reconciled.Store(true)
	m.workers = map[string]*workerConn{w.ip: w}
	m.workerOrder = []string{w.ip}
	t.Cleanup(func() { close(w.done) })
	return w
}

func TestPreloadPass(t *testing.T) {
	f := newPreloadFixture(t)
	m := f.m
	repo := preloadTestRegistry + "/test/"
	d := newFakeImageDaemon(t,
		image.Summary{ID: "old-web", RepoTags: []string{repo + "web:s1-aaaa-challenge"}},
		image.Summary{ID: "old-other", RepoTags: []string{repo + "other:s1-bbbb-challenge"}},
		image.Summary{ID: "other", RepoTags: []string{f.otherName}},
	)
	w := d.conn(t, m)

	m.preloadPass(w)
	pulls, removes := d.calls()
	if want := []string{f.webName}; !reflect.DeepEqual(pulls, want) {
		t.Errorf("pulls = %v, want %v", pulls, want)
	}
	// Only the preloaded challenge's old tag: test/other is not preloaded.
	if want := []string{repo + "web:s1-aaaa-challenge force=0"}; !reflect.DeepEqual(removes, want) {
		t.Errorf("removes = %v, want %v", removes, want)
	}

	m.preloadPass(w)
	if again, removedAgain := d.calls(); len(again) != 1 || len(removedAgain) != 1 {
		t.Errorf("a second pass pulled %v and removed %v, want nothing new", again, removedAgain)
	}

	infos, err := m.ListPreloads()
	if err != nil {
		t.Fatalf("ListPreloads: %s", err)
	}
	if len(infos) != 1 || infos[0].Schema != "event" || infos[0].Images != 1 || len(infos[0].Workers) != 1 {
		t.Fatalf("ListPreloads = %+v", infos)
	}
	if got := infos[0].Workers[0]; got.Held != 1 || got.LastPass == nil || got.Reachable != "ok" || len(got.Failed) > 0 {
		t.Errorf("worker status = %+v, want 1 held, checked, nothing failed", got)
	}
}

// A schema nobody preloads costs a worker one query a pass and no daemon call.
func TestPreloadPassWithoutPreloads(t *testing.T) {
	f := newPreloadFixture(t)
	if _, err := f.m.RemovePreload("event"); err != nil {
		t.Fatalf("RemovePreload: %s", err)
	}
	d := newFakeImageDaemon(t, image.Summary{ID: "old-web", RepoTags: []string{preloadTestRegistry + "/test/web:s1-aaaa-challenge"}})
	f.m.preloadPass(d.conn(t, f.m))
	if pulls, removes := d.calls(); len(pulls)+len(removes) > 0 {
		t.Errorf("a pass with nothing preloaded pulled %v and removed %v", pulls, removes)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) > 0 {
		t.Errorf("a pass with nothing preloaded called the daemon: %v", d.requests)
	}
}

func TestKickPreloadsNeverBlocks(t *testing.T) {
	f := newPreloadFixture(t)
	d := newFakeImageDaemon(t)
	d.conn(t, f.m)
	done := make(chan struct{})
	go func() {
		f.m.kickPreloads()
		f.m.kickPreloads() // the buffer is full: dropped, not waited on
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("kickPreloads blocked on a worker nobody is draining")
	}
}

// Deletes that outlast the control timeout are a busy daemon, not a dead one:
// they must not count against the worker, and the phase stops at the first.
func TestPreloadTimeoutsDoNotEjectTheWorker(t *testing.T) {
	f := newPreloadFixture(t)
	timing := defaultWorkerTiming
	timing.controlTimeout = 50 * time.Millisecond
	f.m.workerTiming = timing
	old := []image.Summary{{ID: "web", RepoTags: []string{f.webName}}}
	for j := 0; j < 3; j++ {
		old = append(old, image.Summary{ID: fmt.Sprintf("old-%d", j),
			RepoTags: []string{fmt.Sprintf("%s/test/web:s1-%d-challenge", preloadTestRegistry, j)}})
	}
	d := newFakeImageDaemon(t, old...)
	var attempts atomic.Int32
	d.mu.Lock()
	d.onDelete = func() { attempts.Add(1); time.Sleep(300 * time.Millisecond) }
	d.mu.Unlock()
	w := d.conn(t, f.m)

	f.m.removeSuperseded(w, d.tags(), nil)
	time.Sleep(50 * time.Millisecond)
	if n := attempts.Load(); n != 1 {
		t.Errorf("%d delete(s) attempted, want the phase to stop after the first timed out", n)
	}
	if r := workerReachable(w.reachable.Load()); r != workerReachableOk {
		t.Errorf("worker is %s after preload deletes timed out, want ok", r)
	}
}

// A preload ends with preload-remove or the schema's deletion, not with an
// update that leaves the schema empty (a converge that failed): the schema
// comes back preloaded, and meanwhile preload-list shows it holding nothing.
func TestAPreloadOutlivesAnEmptiedSchema(t *testing.T) {
	f := newPreloadFixture(t)
	if errs := f.m.UpdateSchema(&Schema{Name: "event", FlagFormat: "flag{%s}", Challenges: map[ChallengeId]BuildSpecification{}}); len(errs) > 0 {
		t.Fatalf("UpdateSchema: %v", errs)
	}
	infos, err := f.m.ListPreloads()
	if err != nil {
		t.Fatalf("ListPreloads: %s", err)
	}
	if len(infos) != 1 || infos[0].Schema != "event" || infos[0].Images != 0 {
		t.Errorf("preloads after an update emptied the schema = %+v, want event with no images", infos)
	}
	if !f.m.preloading.Load() {
		t.Error("an emptied schema's preload left the fleet marked as preloading nothing")
	}
}

// At most CORK_PRELOAD_PULLS pulls run at once across the fleet.
func TestPreloadPullsAreCappedAcrossTheFleet(t *testing.T) {
	f := newPreloadFixture(t)
	f.m.preloadPulls = make(chan struct{}, 1)
	var inFlight, most atomic.Int32
	hook := func() {
		n := inFlight.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
	}
	var conns []*workerConn
	for i := 0; i < 3; i++ {
		d := newFakeImageDaemon(t)
		d.mu.Lock()
		d.onPull = hook
		d.mu.Unlock()
		conns = append(conns, d.conn(t, f.m))
	}
	var wg sync.WaitGroup
	for _, w := range conns {
		wg.Add(1)
		go func(w *workerConn) { defer wg.Done(); f.m.preloadPass(w) }(w)
	}
	wg.Wait()
	if got := most.Load(); got != 1 {
		t.Errorf("%d preload pulls ran at once with a cap of 1", got)
	}
}

// A pull that waited for a fleet slot goes ahead only if its worker is still
// placeable once it has one.
func TestAPreloadPullChecksItsWorkerAfterTheSlotWait(t *testing.T) {
	f := newPreloadFixture(t)
	f.m.preloadPulls = make(chan struct{}, 1)
	f.m.preloadPulls <- struct{}{} // another worker's pull
	d := newFakeImageDaemon(t)
	w := d.conn(t, f.m)
	done := make(chan struct{})
	go func() { f.m.preloadPass(w); close(done) }()
	eventually(t, time.Second, "the pass to list the worker", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.requests) > 0
	})
	time.Sleep(20 * time.Millisecond) // into the slot wait
	w.reachable.Store(int32(workerUnresponsive))
	<-f.m.preloadPulls
	<-done
	if pulls, _ := d.calls(); len(pulls) > 0 {
		t.Errorf("pulled %v onto a worker that left placement during the slot wait", pulls)
	}
}

// preload-list shows a pull under the schema it is for, not under every one.
func TestPreloadStatusShowsOnlyItsSchemasPull(t *testing.T) {
	var s preloadStatus
	s.begin(map[string]bool{})
	s.pull("r/other:1")
	if got := s.report("w", workerReachableOk, []string{"r/web:1"}); got.Pulling != "" {
		t.Errorf("a schema not pulling anything shows %q", got.Pulling)
	}
	if got := s.report("w", workerReachableOk, []string{"r/other:1"}); got.Pulling != "r/other:1" {
		t.Errorf("the schema being pulled for shows %q", got.Pulling)
	}
}

// Startup registers its workers under the lock: each conn's goroutine starts
// as it is made, and may read the map.
func TestInitWorkersRegistersUnderTheLock(t *testing.T) {
	m := setupTestManager(t)
	t.Cleanup(func() { m.db.Close() })
	m.ctx = context.Background()
	m.workerTiming = probeTiming()
	for i := 1; i <= 50; i++ {
		m.db.MustExec("INSERT INTO workers(ip, public) VALUES (?, '');", fmt.Sprintf("127.0.0.%d", i))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				m.kickPreloads() // iterates the workers, as a build landing would
			}
		}
	}()
	if err := m.initWorkers(); err != nil {
		t.Fatalf("initWorkers: %s", err)
	}
	close(stop)
	wg.Wait()
	m.workersMu.Lock()
	for _, w := range m.workers {
		close(w.done)
	}
	m.workersMu.Unlock()
}

// A build's new generation goes out as it lands, while preloading is in play,
// rather than when the operation it belongs to ends.
func TestABuildLandingKicksTheWorkers(t *testing.T) {
	f := newPreloadFixture(t)
	d := newFakeImageDaemon(t)
	w := d.conn(t, f.m)
	build, err := f.m.lookupBuildMetadata(f.web)
	if err != nil {
		t.Fatalf("lookupBuildMetadata: %s", err)
	}
	build.Checksum++
	if err := f.m.finalizeBuild(build); err != nil {
		t.Fatalf("finalizeBuild: %s", err)
	}
	if len(w.preloadNow) != 1 {
		t.Error("a preloaded build landed without kicking the workers")
	}
	<-w.preloadNow
	if _, err := f.m.RemovePreload("event"); err != nil {
		t.Fatalf("RemovePreload: %s", err)
	}
	build.Checksum++
	if err := f.m.finalizeBuild(build); err != nil {
		t.Fatalf("finalizeBuild: %s", err)
	}
	if len(w.preloadNow) != 0 {
		t.Error("a build landed on a fleet preloading nothing and kicked the workers")
	}
}

func TestStaleTag(t *testing.T) {
	ref := preloadTestRegistry + "/test/web:s1-aaaa-challenge"
	stale := staleTag(ref)
	if want := preloadTestRegistry + "/test/web:stale-s1-aaaa-challenge"; stale != want {
		t.Errorf("staleTag(%q) = %q, want %q", ref, stale, want)
	}
	if isStale(ref) || !isStale(stale) {
		t.Errorf("isStale: %q = %v, %q = %v", ref, isStale(ref), stale, isStale(stale))
	}
}

// A superseded image a container still uses is not forced off: dockerd refuses
// the unforced delete, and it is kept under its stale tag instead, which a
// later pass deletes once nothing uses it.
func TestAnImageInUseIsKeptAsStale(t *testing.T) {
	f := newPreloadFixture(t)
	old := preloadTestRegistry + "/test/web:s1-aaaa-challenge"
	stale := staleTag(old)
	d := newFakeImageDaemon(t,
		image.Summary{ID: "web", RepoTags: []string{f.webName}},
		image.Summary{ID: "old-web", RepoTags: []string{old}},
	)
	d.inUse["old-web"] = true
	w := d.conn(t, f.m)

	f.m.removeSuperseded(w, d.tags(), nil)
	if got, want := d.tags(), map[string]bool{f.webName: true, stale: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags after the refused delete = %v, want %v", got, want)
	}
	f.m.removeSuperseded(w, d.tags(), nil) // still in use
	if got, want := d.tags(), map[string]bool{f.webName: true, stale: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("tags while still in use = %v, want %v", got, want)
	}
	d.mu.Lock()
	tagged := slices.Clone(d.tagged)
	d.inUse["old-web"] = false
	d.mu.Unlock()
	if len(tagged) != 1 {
		t.Errorf("tagged %v, want the one stale tag", tagged)
	}
	f.m.removeSuperseded(w, d.tags(), nil)
	if got, want := d.tags(), map[string]bool{f.webName: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("tags once nothing uses the image = %v, want %v", got, want)
	}
	_, removes := d.calls()
	for _, r := range removes {
		if strings.HasSuffix(r, "force=1") {
			t.Errorf("forced a removal: %s", r)
		}
	}
}

// A pass that fails goes again soon, and less often while it keeps failing.
func TestAFailedPassIsRetriedWithBackoff(t *testing.T) {
	f := newPreloadFixture(t)
	retry := preloadRetryAfter
	preloadRetryAfter = 10 * time.Millisecond
	t.Cleanup(func() { preloadRetryAfter = retry })
	d := newFakeImageDaemon(t)
	d.mu.Lock()
	d.pullFails = true
	d.mu.Unlock()
	w := d.conn(t, f.m)
	go f.m.preloadLoop(w)
	time.Sleep(400 * time.Millisecond)
	pulls := 0
	d.mu.Lock()
	for _, r := range d.requests {
		if strings.HasSuffix(r, "/images/create") {
			pulls++
		}
	}
	d.mu.Unlock()
	// Retries at 10, 20, 40, 80, 160ms make six attempts in 400ms; every
	// 10ms would make forty.
	if pulls < 2 || pulls > 12 {
		t.Errorf("%d pull attempts in 400ms, want a retry soon that backs off", pulls)
	}
}

// A pass that could not list the worker counts as failed, and is retried.
func TestAPassThatCannotListTheWorkerFails(t *testing.T) {
	f := newPreloadFixture(t)
	d := newFakeImageDaemon(t)
	d.mu.Lock()
	d.listFails = true
	d.mu.Unlock()
	if !f.m.preloadPass(d.conn(t, f.m)) {
		t.Error("a pass that could not list the worker reported nothing to retry")
	}
}

// preload-remove reads what it covered itself: a flag that missed a preload
// (an earlier failed read) must not turn its answer into "nothing released".
func TestRemovePreloadDoesNotTrustThePreloadingFlag(t *testing.T) {
	f := newPreloadFixture(t)
	f.m.preloading.Store(false)
	got, err := f.m.RemovePreload("event")
	if err != nil {
		t.Fatalf("RemovePreload: %s", err)
	}
	if want := []string{f.webName}; !reflect.DeepEqual(got, want) {
		t.Errorf("released %v, want %v", got, want)
	}
}

// preload-add kicks the workers, as each landing build does; other operations
// leave what they change to the sweep.
func TestOnlyAPreloadAddKicks(t *testing.T) {
	f := newPreloadFixture(t)
	d := newFakeImageDaemon(t)
	w := d.conn(t, f.m)
	f.m.lockUpdates()
	f.m.unlockUpdates()
	if len(w.preloadNow) != 0 {
		t.Error("an operation that landed no build kicked the worker")
	}
	if err := f.m.AddPreload("nonesuch"); err == nil {
		t.Fatal("AddPreload of an unknown schema succeeded")
	}
	if len(w.preloadNow) != 0 {
		t.Error("a failed preload-add kicked the worker")
	}
	if err := f.m.AddPreload("archive"); err != nil {
		t.Fatalf("AddPreload: %s", err)
	}
	if len(w.preloadNow) != 1 {
		t.Error("preload-add did not kick the worker")
	}
}

// A version that lands while a pass runs is pulled by the next, and the old
// ones wait for it there, rather than going first and taking the layers it
// shares with them.
func TestAnOldImageWaitsForASuccessorThatLandsMidPass(t *testing.T) {
	f := newPreloadFixture(t)
	old := preloadTestRegistry + "/test/web:s1-aaaa-challenge"
	d := newFakeImageDaemon(t, image.Summary{ID: "old-web", RepoTags: []string{old}})
	w := d.conn(t, f.m)
	var once sync.Once
	d.mu.Lock()
	d.onPull = func() {
		once.Do(func() {
			build, err := f.m.lookupBuildMetadata(f.web)
			if err != nil {
				t.Errorf("lookupBuildMetadata: %s", err)
				return
			}
			build.Checksum++
			if err := f.m.finalizeBuild(build); err != nil {
				t.Errorf("finalizeBuild: %s", err)
			}
		})
	}
	d.mu.Unlock()
	f.m.preloadPass(w)
	if _, removes := d.calls(); len(removes) > 0 {
		t.Errorf("removed %v before the version that landed mid-pass was pulled", removes)
	}
	build, err := f.m.lookupBuildMetadata(f.web)
	if err != nil {
		t.Fatalf("lookupBuildMetadata: %s", err)
	}
	landed := f.m.instanceImageName(build.Challenge, build, Image{Host: "challenge"})
	f.m.preloadPass(w)
	pulls, removes := d.calls()
	if !slices.Contains(pulls, landed) {
		t.Errorf("pulls = %v, want the version that landed, %s", pulls, landed)
	}
	if want := []string{f.webName + " force=0", old + " force=0"}; !reflect.DeepEqual(removes, want) {
		t.Errorf("removes once it is held = %v, want %v", removes, want)
	}
}

// A failed pull does not hold the old version back: on a full disk, deleting
// it is what makes room for the retry.
func TestAFailedPullReleasesTheOldVersion(t *testing.T) {
	f := newPreloadFixture(t)
	old := preloadTestRegistry + "/test/web:s1-aaaa-challenge"
	d := newFakeImageDaemon(t, image.Summary{ID: "old-web", RepoTags: []string{old}})
	d.mu.Lock()
	d.pullFails = true
	d.mu.Unlock()
	f.m.preloadPass(d.conn(t, f.m))
	if _, removes := d.calls(); !reflect.DeepEqual(removes, []string{old + " force=0"}) {
		t.Errorf("removes after the new version's pull failed = %v, want %s", removes, old)
	}
}

// A pass pulls nothing that stopped being wanted while it ran: preload-remove
// takes effect on a pass already pulling.
func TestAPassPullsNothingNoLongerWanted(t *testing.T) {
	f := newPreloadFixture(t)
	more := &BuildMetadata{
		Id: insertTestBuild(t, f.m, "event", "test/web", "flag{%s}", 3, 0x5555), Flag: "flag{x}",
		Challenge: "test/web", Seed: 3, Checksum: 0x5555, Images: []Image{{Host: "challenge", Ports: []string{}}},
	}
	if err := f.m.finalizeBuild(more); err != nil {
		t.Fatalf("finalizeBuild: %s", err)
	}
	d := newFakeImageDaemon(t)
	var once sync.Once
	d.mu.Lock()
	d.onPull = func() {
		once.Do(func() {
			if _, err := f.m.RemovePreload("event"); err != nil {
				t.Errorf("RemovePreload: %s", err)
			}
		})
	}
	d.mu.Unlock()
	f.m.preloadPass(d.conn(t, f.m))
	if pulls, _ := d.calls(); len(pulls) != 1 {
		t.Errorf("pulled %v, want only the pull under way when the preload was removed", pulls)
	}
}

// A timeout marking an image stale ends the worker's removals, as a delete's
// does: more calls would only queue behind a slow daemon.
func TestATimeoutMarkingStaleEndsTheRemovals(t *testing.T) {
	f := newPreloadFixture(t)
	timing := defaultWorkerTiming
	timing.controlTimeout = 50 * time.Millisecond
	f.m.workerTiming = timing
	images := []image.Summary{{ID: "web", RepoTags: []string{f.webName}}}
	d := newFakeImageDaemon(t)
	for j := 0; j < 3; j++ {
		id := fmt.Sprintf("old-%d", j)
		images = append(images, image.Summary{ID: id, RepoTags: []string{fmt.Sprintf("%s/test/web:s1-%d-challenge", preloadTestRegistry, j)}})
		d.inUse[id] = true
	}
	d.mu.Lock()
	d.images = images
	d.onDelete = func() { time.Sleep(300 * time.Millisecond) } // the untag after the stale tag
	d.mu.Unlock()
	w := d.conn(t, f.m)
	f.m.removeSuperseded(w, d.tags(), nil)
	if _, removes := d.calls(); len(removes) != 2 {
		t.Errorf("deletes = %v, want the refused one and the untag that timed out", removes)
	}
}

// A worker out of placement is not preloaded, and one that leaves during its
// removals stops them.
func TestAnUnreachableWorkerIsLeftAlone(t *testing.T) {
	f := newPreloadFixture(t)
	d := newFakeImageDaemon(t)
	w := d.conn(t, f.m)
	w.reachable.Store(int32(workerUnresponsive))
	f.m.preloadPass(w)
	d.mu.Lock()
	requests := slices.Clone(d.requests)
	d.mu.Unlock()
	if len(requests) > 0 {
		t.Errorf("a pass on an unresponsive worker called its daemon: %v", requests)
	}

	images := []image.Summary{{ID: "web", RepoTags: []string{f.webName}}}
	for j := 0; j < 3; j++ {
		images = append(images, image.Summary{ID: fmt.Sprintf("old-%d", j),
			RepoTags: []string{fmt.Sprintf("%s/test/web:s1-%d-challenge", preloadTestRegistry, j)}})
	}
	d.mu.Lock()
	d.images = images
	d.onDelete = func() { w.reachable.Store(int32(workerUnresponsive)) }
	d.mu.Unlock()
	w.reachable.Store(int32(workerReachableOk))
	f.m.removeSuperseded(w, d.tags(), nil)
	if _, removes := d.calls(); len(removes) != 1 {
		t.Errorf("deletes = %v, want the removals to stop once the worker left", removes)
	}
}

// A pass that succeeds after a run of failures puts the retry back to its
// shortest: the next failure is retried soon, not at the backoff reached.
func TestASucceedingPassResetsTheBackoff(t *testing.T) {
	f := newPreloadFixture(t)
	retry := preloadRetryAfter
	preloadRetryAfter = 20 * time.Millisecond
	t.Cleanup(func() { preloadRetryAfter = retry })
	d := newFakeImageDaemon(t)
	pullsTried := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		n := 0
		for _, r := range d.requests {
			if strings.HasSuffix(r, "/images/create") {
				n++
			}
		}
		return n
	}
	d.mu.Lock()
	d.pullFails = true
	d.mu.Unlock()
	w := d.conn(t, f.m)
	go f.m.preloadLoop(w)
	time.Sleep(700 * time.Millisecond) // retries at 20, 40, 80, 160, 320ms: the next is 640ms off
	d.mu.Lock()
	d.pullFails = false
	d.mu.Unlock()
	w.preloadNow <- struct{}{}
	eventually(t, 5*time.Second, "the pass after the failures to pull", func() bool {
		pulls, _ := d.calls()
		return len(pulls) == 1
	})
	time.Sleep(50 * time.Millisecond) // into the wait after the pass
	d.mu.Lock()
	d.images, d.pullFails = nil, true
	d.mu.Unlock()
	before := pullsTried()
	w.preloadNow <- struct{}{}
	time.Sleep(250 * time.Millisecond)
	if n := pullsTried() - before; n < 2 {
		t.Errorf("%d pull attempt(s) in the 250ms after a new failure, want the retry back at 20ms", n)
	}
}

// A pass's failures are its own: the next pass starts with none.
func TestAPassStartsWithNoFailures(t *testing.T) {
	var s preloadStatus
	s.begin(map[string]bool{})
	s.pulled("r/web:1", errors.New("manifest unknown"))
	s.begin(map[string]bool{})
	if got := s.report("w", workerReachableOk, []string{"r/web:1"}); len(got.Failed) > 0 {
		t.Errorf("a new pass reports %v, an earlier pass's failure", got.Failed)
	}
}

// A worker admitted to placement gets its preloader.
func TestAnAdmittedWorkerPreloads(t *testing.T) {
	f := newPreloadFixture(t)
	f.m.workerTiming = probeTiming()
	d := newFakeImageDaemon(t)
	w := d.conn(t, f.m)
	w.reachable.Store(int32(workerUnresponsive)) // as newWorkerConn leaves it
	w.reconciled.Store(false)
	go f.m.runWorker(w)
	eventually(t, 5*time.Second, "the admitted worker to pull what is preloaded", func() bool {
		pulls, _ := d.calls()
		return slices.Contains(pulls, f.webName)
	})
}

// A pass that cannot read the database pulls nothing on a guess: a read that
// fails before a pull, after the wait for its slot, ends the pass as failed.
func TestAPassPullsNothingItCouldNotRead(t *testing.T) {
	f := newPreloadFixture(t)
	f.m.preloadPulls = make(chan struct{}, 1)
	f.m.preloadPulls <- struct{}{} // another worker's pull
	d := newFakeImageDaemon(t)
	w := d.conn(t, f.m)
	failed := make(chan bool)
	go func() { failed <- f.m.preloadPass(w) }()
	eventually(t, time.Second, "the pass to list the worker", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.requests) > 0
	})
	time.Sleep(20 * time.Millisecond) // into the slot wait
	f.m.db.MustExec("DROP TABLE preloads;")
	<-f.m.preloadPulls
	if !<-failed {
		t.Error("a pass that could not read what to pull reported nothing to retry")
	}
	if pulls, _ := d.calls(); len(pulls) > 0 {
		t.Errorf("pulled %v without reading whether it was still wanted", pulls)
	}
}

// A pass whose removals cannot read the database is retried, as one whose
// pulls could not is.
func TestAPassThatCannotReadWhatItSupersedesFails(t *testing.T) {
	f := newPreloadFixture(t)
	d := newFakeImageDaemon(t, image.Summary{ID: "web", RepoTags: []string{f.webName}})
	d.mu.Lock()
	d.onList = func() { f.m.db.MustExec("DROP TABLE preloads;") }
	d.mu.Unlock()
	if !f.m.preloadPass(d.conn(t, f.m)) {
		t.Error("a pass whose removals could not read the database reported nothing to retry")
	}
}
