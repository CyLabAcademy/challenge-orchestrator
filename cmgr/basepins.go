package cmgr

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// Base image pinning.
//
// BuildKit resolves a mutable tag such as `FROM ubuntu:24.04` against the
// registry on essentially every build, where the legacy builder simply used
// whatever copy was already local. Measured on a fleet-shaped corpus that is
// tens of registry round trips per update pass, which at fleet scale is both
// Docker Hub rate-limit exposure and a silent dependency on whatever Hub is
// serving that minute.
//
// Rewriting `FROM name:tag` to `FROM name@sha256:...` removes both: a digest
// needs no resolution, and the base only moves when someone deliberately
// refreshes the pin. cork does the rewrite while it is already synthesizing
// the build context, so challenge Dockerfiles keep their readable tags and
// several hundred of them need no edit.
//
// The map lives in a JSON file (CORK_BASE_PINS, default <CORK_DIR>/.base-pins.json):
//
//	{"ubuntu:24.04": "sha256:...", "nginx:mainline": "sha256:..."}
//
// Absent or empty means no pinning, and every Dockerfile is passed through
// untouched.
//
// What a refresh does and does not do. The pin map is fingerprinted into
// contentChecksum, so every image built after a refresh carries a different
// content identity than it would have before: a moved base can never collide
// with the old one on a tag. It is not a rebuild trigger. DetectChanges
// compares only the challenge directory's own checksums, so a refresh leaves
// every challenge Unmodified and `update` rebuilds nothing. A base moves onto
// a challenge when that challenge is next rebuilt for its own reasons.
//
// The practical consequence, and it is the one to know: refreshing pins before
// a batch update moves the base for the challenges in that batch, and leaves
// every untouched challenge on the base it was last built with. Rolling the
// whole fleet onto a new base needs a rebuild of the whole fleet, which cork
// has no single command for today. See BUILDER.md.

const BASE_PINS_ENV = "CORK_BASE_PINS"

// fromLineRe splits a FROM instruction into its prefix, any flags such as
// --platform=, the image reference, and whatever follows (typically `AS name`).
var fromLineRe = regexp.MustCompile(`(?i)^(\s*FROM\s+)((?:--\S+\s+)*)(\S+)(.*)$`)

// asClauseRe pulls the stage alias out of the tail of a FROM instruction.
var asClauseRe = regexp.MustCompile(`(?i)^\s+AS\s+(\S+)\s*$`)

// refRe is what an image reference can look like. Deliberately permissive
// about registries, ports and tags, and deliberately anchored, so that a token
// which is not a reference at all -- a lone backslash from a continued line, a
// fragment of shell -- is never offered to the registry or written into the pin
// map. Note this is also what rejects every `${VAR}` form, so the explicit `$`
// case in the scanner is belt and braces rather than the actual guard.
var refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)

// heredocMarker is the one thing this scanner refuses to reason about.
//
// A Dockerfile heredoc (`RUN <<EOF` ... `EOF`) makes the lines between the
// marker and its terminator SCRIPT rather than Dockerfile syntax -- and `FROM`
// is an ordinary word in shell, Python and SQL. Getting that wrong in the
// unsafe direction rewrites a challenge's own source into
// `from redis@sha256:... import Redis`, which builds clean and fails at
// runtime.
//
// Two attempts at modelling heredocs with a regexp were each wrong in ways
// that were only found by diffing against BuildKit's real parser. It accepts
// whitespace after the marker (`cat << EOF`), the empty-quote delimiter forms
// (`<<""EOF`), delimiters containing `.` and `-`, terminators that must NOT be
// whitespace-trimmed, and it keeps a line continuation open across comments and
// blank lines. Each of those was a way for a body line to be rewritten.
//
// So this does not model them. A Dockerfile containing `<<` anywhere is refused
// whole: no references harvested, nothing rewritten, and the caller says which
// file. That is deliberately blunter than the syntax -- a C++ challenge writing
// `cout<<flag` into a file is refused too -- and the trade is easy: an unpinned
// base costs one registry lookup per build, a wrongly rewritten line ships a
// broken challenge. It also costs nothing to measure: of the 411 Dockerfiles
// in the challenge corpus, ZERO contain `<<`.
//
// If that stops being true, the answer is BuildKit's own parser
// (frontend/dockerfile/parser), not a third regexp.
const heredocMarker = "<<"

// baseRef is one FROM instruction naming an image outside this Dockerfile.
type baseRef struct {
	// line is the index of the physical line to rewrite, or -1 when the
	// instruction spans a line continuation and cannot be rewritten in place.
	line                     int
	prefix, flags, ref, rest string
}

// scanFromInstructions finds the external base images a Dockerfile builds on.
//
// The two callers -- the rewriter that pins and the scanner that decides what
// to resolve -- share this so they cannot disagree about what counts as a base
// image. They used to carry separate copies of the rules, which is how a line
// could be proposed for pinning by one and handled differently by the other.
//
// Excluded, in the order the checks apply:
//
//   - anything inside a heredoc. `RUN <<EOF` bodies are shell, Python, SQL --
//     not Dockerfile syntax -- and FROM is an ordinary word in all three. A
//     Python `from redis import Redis` matched the FROM pattern, which meant
//     the scanner proposed `redis` as a base image and the rewriter then
//     edited a line of the challenge's own source into `from redis@sha256:...
//     import Redis`. That produced a challenge that built cleanly and failed
//     at runtime. BuildKit is what makes heredocs usable in the first place,
//     so this stops being hypothetical the moment an author writes one.
//   - comments, which cannot carry an instruction;
//   - stages defined earlier in this file, by alias, case-insensitively;
//   - `scratch`, the empty base, which is not an image and cannot be resolved;
//   - references already pinned to a digest, and any whose text comes from a
//     build arg, since neither names a fixed tag to resolve.
//
// A FROM split across a line continuation is returned with line == -1: its
// reference is real and worth resolving, but the instruction does not sit on
// one line, so the rewriter leaves it alone rather than guess.
func scanFromInstructions(lines []string) ([]baseRef, bool) {
	var out []baseRef
	stages := map[string]bool{}
	continued := false

	for i, line := range lines {
		// See heredocMarker. One occurrence anywhere refuses the file.
		//
		// The trailing-`<` test is the same rule on the joined line: BuildKit
		// concatenates a continuation with NO separator, so `RUN cat <\\` then
		// `<EOF` is a real heredoc that neither physical line contains `<<`.
		// Verified against BuildKit rather than reasoned about.
		if strings.Contains(line, heredocMarker) ||
			strings.HasSuffix(strings.TrimSuffix(strings.TrimRight(line, " \t\r"), "\\"), "<") {
			return nil, false
		}

		body := strings.TrimRight(line, " 	\r")

		// A blank or comment line does not continue anything and cannot carry
		// an instruction. BuildKit keeps an open continuation running ACROSS
		// these rather than ending it (parser.go, isComment /
		// isEmptyContinuationLine), so they are skipped without touching the
		// flag either way -- an earlier version cleared it here and ended the
		// instruction early, exposing the rest of a RUN body as Dockerfile.
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		wasContinued := continued
		continued = strings.HasSuffix(body, "\\")
		if wasContinued {
			continue // a later physical line of the instruction above
		}

		match := fromLineRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		prefix, flags, ref, rest := match[1], match[2], match[3], match[4]

		// Record this stage's alias before testing the reference, so a later
		// `FROM base` resolves to the stage rather than to an image. Stage
		// names are case-insensitive to BuildKit, so both sides fold.
		if as := asClauseRe.FindStringSubmatch(rest); as != nil {
			stages[strings.ToLower(as[1])] = true
		}

		switch {
		case stages[strings.ToLower(ref)]:
			continue
		case strings.EqualFold(ref, "scratch"):
			continue
		case strings.Contains(ref, "@"):
			continue
		case strings.Contains(ref, "$"):
			continue
		case !refRe.MatchString(ref):
			continue
		}

		idx := i
		if continued {
			idx = -1
		}
		out = append(out, baseRef{line: idx, prefix: prefix, flags: flags, ref: ref, rest: rest})
	}

	return out, true
}

type basePins struct {
	mu   sync.RWMutex
	path string
	m    map[string]string
}

func (b *basePins) snapshot() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]string, len(b.m))
	for k, v := range b.m {
		out[k] = v
	}
	return out
}

func (b *basePins) replace(m map[string]string) {
	b.mu.Lock()
	b.m = m
	b.mu.Unlock()
}

// initBasePins resolves the pin file's location and loads it if present. A
// missing file is not an error: pinning is opt-in.
func (m *Manager) initBasePins() error {
	path, isSet := LookupEnv(BASE_PINS_ENV)
	if !isSet {
		path = filepath.Join(m.chalDir, ".base-pins.json")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		m.log.errorf("could not resolve base pin file: %s", err)
		return err
	}
	m.basePins = &basePins{path: abs, m: map[string]string{}}

	pins, err := readBasePins(abs)
	if err != nil {
		// A malformed file is worth failing on: silently building against
		// mutable tags is exactly what pinning exists to prevent.
		m.log.errorf("could not read base pins from %s: %s", abs, err)
		return err
	}
	m.basePins.replace(pins)
	if len(pins) == 0 {
		// Said out loud. An empty map is indistinguishable at a glance from a
		// working one, and it means every build resolves a mutable tag -- the
		// exposure pinning exists to remove -- while also fingerprinting
		// differently from a pinned deployment. A typo in CORK_BASE_PINS or a
		// `git clean -xdf` over the default dotfile both land here.
		m.log.warnf("no base image pins loaded from %s: builds will resolve mutable tags against the registry", m.basePins.path)
	}
	if len(pins) > 0 {
		m.log.infof("loaded %d base image pin(s) from %s", len(pins), abs)
	}
	return nil
}

func readBasePins(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]string{}, nil
	}
	var pins map[string]string
	if err := json.Unmarshal(data, &pins); err != nil {
		return nil, err
	}
	for ref, digest := range pins {
		if !strings.HasPrefix(digest, "sha256:") {
			return nil, fmt.Errorf("pin for %q is not a sha256 digest: %q", ref, digest)
		}
	}
	return pins, nil
}

// writeBasePins replaces the pin file atomically. A plain truncating write is
// not safe here: readBasePins treats a malformed file as fatal to startup (see
// initBasePins), so a refresh interrupted mid-write -- an OOM kill, a reboot --
// leaves truncated JSON and an orchestrator that will not start until a human
// deletes the file. Rename within the same directory is atomic, so a reader
// sees either the old map or the new one, never half of either.
func writeBasePins(path string, pins map[string]string) error {
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename below has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Durable before the rename: otherwise a crash can leave the real name
	// pointing at a file whose contents never reached the disk, which is the
	// same unstartable state by a slower route.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; the pin map is not a secret and the
	// previous implementation wrote 0644.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// lookupPin finds a pin for the reference as written, tolerating an omitted
// tag the way docker does.
func lookupPin(pins map[string]string, ref string) (string, bool) {
	if d, ok := pins[ref]; ok {
		return d, true
	}
	if !refHasTag(ref) {
		if d, ok := pins[ref+":latest"]; ok {
			return d, true
		}
	}
	return "", false
}

// refHasTag reports whether the reference carries a tag, being careful that a
// colon in a registry's host:port is not one.
func refHasTag(ref string) bool {
	colon := strings.LastIndex(ref, ":")
	return colon > strings.LastIndex(ref, "/")
}

func refName(ref string) string {
	if refHasTag(ref) {
		return ref[:strings.LastIndex(ref, ":")]
	}
	return ref
}

// pinDockerfile rewrites every FROM whose image is pinned, and reports which
// pins it applied. Internal stage references, already-digested references, and
// ARG-driven ones are left alone.
// The third return is false when the file could not be parsed with confidence
// (an unterminated heredoc). Nothing is rewritten in that case.
func pinDockerfile(dockerfile []byte, pins map[string]string) ([]byte, []string, bool) {
	if len(pins) == 0 || len(dockerfile) == 0 {
		return dockerfile, nil, true
	}

	lines := strings.Split(string(dockerfile), "\n")
	var applied []string

	froms, clean := scanFromInstructions(lines)
	if !clean {
		return dockerfile, nil, false
	}
	for _, from := range froms {
		if from.line < 0 {
			// The instruction spans a line continuation, so there is no single
			// line to rewrite. Left on its mutable tag rather than guessed at.
			continue
		}
		digest, ok := lookupPin(pins, from.ref)
		if !ok {
			continue
		}
		lines[from.line] = from.prefix + from.flags + refName(from.ref) + "@" + digest + from.rest
		applied = append(applied, from.ref)
	}

	if len(applied) == 0 {
		return dockerfile, nil, true
	}
	sort.Strings(applied)
	return []byte(strings.Join(lines, "\n")), applied, true
}

// checksum is a stable fingerprint of the pin map, so that changing which
// digest a base resolves to changes the identity of every image built on it.
// Empty means no pins, and returns 0 so an unpinned deployment keeps exactly
// the content checksums it had before pinning existed.
func (b *basePins) checksum() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.m) == 0 {
		return 0
	}
	refs := make([]string, 0, len(b.m))
	for ref := range b.m {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	h := crc32.NewIEEE()
	for _, ref := range refs {
		h.Write([]byte(ref))
		h.Write([]byte{0})
		h.Write([]byte(b.m[ref]))
		h.Write([]byte{10})
	}
	sum := h.Sum32()
	if sum == 0 {
		sum = 1
	}
	return sum
}

// basePinsChecksum is the pin fingerprint contentChecksum mixes in, or 0 when
// pinning is off.
func (m *Manager) basePinsChecksum() uint32 {
	if m.basePins == nil {
		return 0
	}
	return m.basePins.checksum()
}

// pinBases applies the current pins to a Dockerfile on its way into a build
// context.
func (m *Manager) pinBases(challenge ChallengeId, dockerfile []byte) []byte {
	if m.basePins == nil {
		return dockerfile
	}
	out, applied, clean := pinDockerfile(dockerfile, m.basePins.snapshot())
	if !clean {
		m.log.warnf("not pinning base images for %s: its Dockerfile contains %q, so the rest of the file cannot be told apart from script; every build of it resolves its bases against the registry", challenge, heredocMarker)
		return out
	}
	if len(applied) > 0 {
		m.log.debugf("pinned base image(s) %s", strings.Join(applied, ", "))
	}
	return out
}

// BasePin is one entry of the pin map, as reported by the API.
type BasePin struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	// InUse is the number of challenge Dockerfiles whose FROM lines name this
	// reference; a pin nothing uses is a candidate for removal.
	InUse int `json:"in_use"`
}

// ListBasePins reports the current pins alongside how many challenges use each.
func (m *Manager) ListBasePins() ([]BasePin, error) {
	if m.basePins == nil {
		return []BasePin{}, nil
	}
	pins := m.basePins.snapshot()
	used, err := m.scanBaseRefs()
	if err != nil {
		return nil, err
	}
	out := make([]BasePin, 0, len(pins))
	for ref, digest := range pins {
		out = append(out, BasePin{Ref: ref, Digest: digest, InUse: used[ref]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// scanBaseRefs walks the challenge directory and counts the external image
// references its Dockerfiles build on, so a refresh can pin exactly those.
//
// Only a Dockerfile sitting beside a problem.md counts, because that is the
// only one cork ever builds: createBuildContext rewrites the challenge's root
// Dockerfile and nothing else. A repository typically also holds Dockerfiles
// that are not challenge roots -- a web challenge's app/ or bot/ subdirectory,
// a directory of hand-maintained base images -- and pinning their bases would
// put references into the fingerprint that no build ever consumes, so that
// moving one of them would re-stamp the identity of the entire fleet for no
// reason.
func (m *Manager) scanBaseRefs() (map[string]int, error) {
	refs := map[string]int{}
	err := filepath.Walk(m.chalDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Not swallowed, for the reason spelled out on the read below: a
			// subtree that drops out of the scan drops out of `used`, and the
			// refresh seeds only from `used`, so an unreadable directory would
			// retire every pin beneath it and re-stamp the fleet's content
			// identity with nothing said.
			return fmt.Errorf("could not walk %s: %w", path, err)
		}
		if info.IsDir() {
			if info.Name()[0] == '.' && path != m.chalDir {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() != "Dockerfile" {
			return nil
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "problem.md")); err != nil {
			return nil // not a challenge root
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// Not swallowed. A file that drops out of the scan drops out of
			// `used`, and RefreshBasePins seeds only from `used` -- so a
			// transient read failure would retire that base's pin, change the
			// pin-map checksum and re-stamp the content identity of the whole
			// fleet, with nothing said. Failing the walk is the conservative
			// half of the same argument that makes the refusal below safe.
			return fmt.Errorf("could not read %s: %w", path, err)
		}
		found, clean := scanFromInstructions(strings.Split(string(data), "\n"))
		if !clean {
			// Named here because this is the only place that knows the path:
			// externalBaseRefs is a pure function and pinBases sees only bytes.
			m.log.warnf("not pinning bases in %s: it contains %q, which makes the rest of the file ambiguous (see basepins.go)", path, heredocMarker)
			return nil
		}
		for _, f := range found {
			refs[f.ref]++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The built-in challenge types ship their own Dockerfiles, and those bases
	// need pinning just as much as the custom ones.
	for _, t := range []string{"flag-only", "remote-make", "static-make"} {
		for ref := range externalBaseRefs(m.GetDockerfile(t)) {
			refs[ref]++
		}
	}
	return refs, nil
}

// externalBaseRefs returns the image references a Dockerfile builds on,
// excluding its own stages and anything already pinned or arg-driven.
func externalBaseRefs(dockerfile []byte) map[string]bool {
	out := map[string]bool{}
	if len(dockerfile) == 0 {
		return out
	}
	froms, clean := scanFromInstructions(strings.Split(string(dockerfile), "\n"))
	if !clean {
		// The same refusal as pinDockerfile, and it has to be the same one: a
		// ref harvested from a body that was misread is a ref that gets
		// resolved against a registry and written into the pin map.
		return out
	}
	for _, from := range froms {
		out[from.ref] = true
	}
	return out
}

// basePinsRefreshBudget is the ceiling on one refresh pass, for n references.
//
// Derived from controlTimeout rather than being its own setting, so the one
// knob an operator already has for "this registry is slow"
// (CORK_WORKER_CONTROL_TIMEOUT) moves both. It allows an average of a fifth of
// a per-call timeout per reference, with a floor of two full timeouts so a
// tiny corpus still gets room for one straggler. At the defaults that is about
// four minutes for a corpus of thirty-odd bases, against a measured normal
// pass of well under one -- generous for a slow registry, and far short of the
// N * controlTimeout an unbounded pass would spend on a stalled one.
func (m *Manager) basePinsRefreshBudget(n int) time.Duration {
	perCall := m.timing().controlTimeout
	// One reference can cost TWO timeouts -- the registry lookup, then the
	// local fallback -- and the loop stops when less than that remains, so the
	// budget has to carry that reserve ON TOP of the work itself. Folding the
	// reserve into the floor instead made the two equal, and a small corpus
	// then gave up on its first iteration having resolved nothing: the e2e,
	// with two references and a ten second timeout, reported "0 of 2".
	reserve := 2 * perCall
	return reserve + max(reserve, time.Duration(n)*perCall/5)
}

// RefreshBasePins resolves every base reference the challenge directory uses to
// the digest the registry serves right now and persists the result. This is the
// one moment cork consults a mutable tag; builds never do.
//
// Existing pins are re-resolved too, so a refresh is how a base is moved.
func (m *Manager) RefreshBasePins() ([]BasePin, error) {
	if m.basePins == nil {
		return nil, fmt.Errorf("base pinning is not configured")
	}
	used, err := m.scanBaseRefs()
	if err != nil {
		return nil, err
	}
	if len(used) == 0 {
		m.log.warn("no base image references found to pin")
	}

	// Seed from what is already pinned, keyed on the refs still in use, so a
	// ref that fails to resolve on this pass KEEPS the digest it had. Building
	// the map from scratch meant one registry blip silently deleted a good pin
	// and sent every build of that base back to a mutable tag: a base moving
	// because the network hiccuped, which is the opposite of what pinning is
	// for. Refs no longer named by any challenge are simply not seeded, which
	// is how a pin for a base that left the corpus is retired.
	existing := m.basePins.snapshot()
	pins := map[string]string{}
	for ref := range used {
		// lookupPin rather than an exact index, because that is how the
		// rewriter reads this map: a pin stored as `ubuntu:latest` covers a
		// `FROM ubuntu`. Seeding by exact key missed those, so one failed
		// resolve deleted a pin that was in use -- the very outcome seeding
		// exists to prevent, through a side door.
		if digest, ok := lookupPin(existing, ref); ok {
			pins[ref] = digest
		}
	}

	// One budget for the whole pass. resolveDigest takes controlTimeout per
	// call, which bounds a single lookup but says nothing about the refresh:
	// against a registry that accepts connections and then stalls, N bases
	// serialize into N * controlTimeout inside a single POST /pins, and
	// neither corkd's server nor cork sets a timeout of its own to cut it
	// short. Exhausting the budget is not a failure of the pins already
	// resolved -- they are written, and the refs left over keep what they had.
	budget := m.basePinsRefreshBudget(len(used))
	deadline := time.Now().Add(budget)
	// One reference can cost two timeouts -- the registry lookup and the local
	// fallback each take their own -- and the deadline is only tested between
	// references, so stopping while less than that remains is what keeps the
	// budget a bound rather than a suggestion.
	reserve := 2 * m.timing().controlTimeout

	var resolved int
	var errs []string
	for ref := range used {
		if time.Until(deadline) < reserve {
			// resolved, not len(pins): pins is pre-seeded with every carried
			// over digest, so counting it would report "32 of 32 resolved"
			// having resolved none -- in the steady state, which is the only
			// state where this fires at all.
			err := fmt.Errorf("refresh gave up after %s with %d of %d references re-resolved; run it again", budget, resolved, len(used))
			m.log.error(err)
			errs = append(errs, err.Error())
			break
		}
		digest, err := m.resolveDigest(ref)
		if err != nil {
			if kept, ok := lookupPin(existing, ref); ok {
				m.log.warnf("could not resolve %s (%s); keeping the pin it already had (%s)", ref, err, kept)
			} else {
				m.log.errorf("could not resolve %s: %s", ref, err)
			}
			errs = append(errs, fmt.Sprintf("%s: %s", ref, err))
			continue
		}
		pins[ref] = digest
		resolved++
		m.log.infof("pinned %s to %s", ref, digest)
	}
	// Only refuse to write when the pass produced nothing at all: no fresh
	// digest and nothing carried over. A partial failure still writes, because
	// what it writes is never worse than what was already there.
	if len(errs) > 0 && len(pins) == 0 {
		return nil, fmt.Errorf("could not resolve any base image: %s", strings.Join(errs, "; "))
	}
	if err := writeBasePins(m.basePins.path, pins); err != nil {
		return nil, err
	}
	m.basePins.replace(pins)

	out, err := m.ListBasePins()
	if err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		// Partial success: the resolved pins are written and in effect, but
		// say which ones are still floating on a mutable tag.
		return out, fmt.Errorf("some references could not be resolved: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// resolveDigest asks the registry what the tag currently points at, without
// downloading the image: a refresh over a corpus with a dozen distinct bases
// should cost a dozen manifest lookups, not a dozen image pulls. The layers
// arrive later, once, when a build actually needs them.
//
// Falls back to a local image's own repo digest, which covers a base that was
// pulled previously but whose registry is momentarily unreachable.
func (m *Manager) resolveDigest(ref string) (string, error) {
	ctx, cancel := m.controlCtx()
	defer cancel()

	dist, err := m.cli.DistributionInspect(ctx, ref, client.DistributionInspectOptions{})
	if err == nil && dist.DistributionInspect.Descriptor.Digest != "" {
		return dist.DistributionInspect.Descriptor.Digest.String(), nil
	}
	registryErr := err

	// Its own budget, not m.ctx: m.ctx is context.Background(), so a wedged
	// local daemon would hang here forever and the per-pass deadline in
	// RefreshBasePins -- which is only ever checked BETWEEN references -- would
	// never come round to notice. A fresh one rather than the context above,
	// which may already have expired getting here.
	ictx, icancel := m.controlCtx()
	defer icancel()
	inspect, localErr := m.cli.ImageInspect(ictx, ref)
	if localErr != nil {
		if registryErr != nil {
			return "", registryErr
		}
		return "", localErr
	}
	name := refName(ref)
	for _, rd := range inspect.RepoDigests {
		if strings.HasPrefix(rd, name+"@") {
			m.log.warnf("using the local digest for %s; the registry did not answer: %s", ref, registryErr)
			return strings.SplitN(rd, "@", 2)[1], nil
		}
	}
	if registryErr != nil {
		return "", registryErr
	}
	return "", fmt.Errorf("no digest for %s (a locally built image has none)", ref)
}
