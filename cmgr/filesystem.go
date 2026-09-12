package cmgr

import (
	"archive/tar"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Resolves the artifacts directory and, on a local build plane, the
// challenge directory from their environment variables.
func (m *Manager) setDirectories() error {
	var err error

	if m.externalBuildPlane {
		// No challenge tree: the build plane has it, and nothing here reads
		// one (DetectChanges and the pins refuse, the converge does not
		// scan). m.chalDir stays empty.
		m.noteIgnoredSetting(DIR_ENV, externalPlaneIgnores)
	} else if err = m.setChallengeDirectory(); err != nil {
		return err
	}

	if m.externalBuildPlane {
		// Artifact bundles are written where the build happens and stay
		// there; a daemon that builds nothing never has one. Nothing below
		// would fail on an external plane -- it would just make an empty
		// directory -- but saying so is the point: an orchestrator's unit
		// file carrying an artifact directory is one whose operator still
		// expects bundles to arrive here.
		m.noteIgnoredSetting(ARTIFACT_DIR_ENV, externalPlaneIgnores)
		return nil
	}

	artifactsDir, isSet := os.LookupEnv(ARTIFACT_DIR_ENV)
	if !isSet {
		artifactsDir = "."
	}

	m.artifactsDir, err = filepath.Abs(artifactsDir)

	if err != nil {
		m.log.errorf("could not resolve artifacts directory: %s", err)
		return err
	}

	m.log.infof("artifacts directory: %s", m.artifactsDir)

	// Created on demand: it only ever holds bundles cmgr writes, so unlike
	// the challenge directory an absent one is not a misconfiguration. A
	// path that exists as a file fails here too.
	if err = os.MkdirAll(m.artifactsDir, 0o755); err != nil {
		m.log.errorf("could not create the artifacts directory: %s", err)
		return err
	}
	return nil
}

// SetArtifactNamespaces says which directory under the artifact directory
// each schema's bundles belong in: schema name to namespace, which a build
// plane fills in from every schema's destination. A schema named here with
// an empty namespace keeps the artifact directory itself -- a single-host
// deployment, or a schema with no destination. A schema not named here at
// all is a different case, and artifactDirForBuild treats it as one.
//
// This is what keeps one build plane's bundles sorted for the several
// orchestrators it builds for: what publishes artifacts to players watches
// one namespace and uploads it under one prefix, so a challenge's files land
// where that destination's players look for them. It is also why a namespace
// is the destination's name exactly, with nothing prepended: the name an
// operator wrote in CORK_DESTINATIONS is the name on disk, and the prefix a
// deployment wants is a destination named that way rather than a convention
// cork invented.
//
// Keyed on the schema rather than set around each build for a reason that
// cost a defect to find: an update rebuilds every build of a challenge whose
// source moved, and those rebuilds happen before any schema is converged and
// span every schema on the plane at once. A "current namespace" moved between
// schemas would send all of them to whichever directory happened to be set,
// while a build row already carries the schema it belongs to.
//
// Called once, before anything is built, and never concurrently with a
// build. Nothing else calls it: an orchestrator holds no bundles.
func (m *Manager) SetArtifactNamespaces(bySchema map[string]string) error {
	namespaces := make(map[string]string, len(bySchema))
	for schema, name := range bySchema {
		if name != "" && (name != filepath.Base(filepath.Clean(name)) || name == "." || name == "..") {
			return fmt.Errorf("schema '%s': artifact namespace %q is not a single directory name", schema, name)
		}
		// Kept even when empty. "This schema belongs in the artifact
		// directory itself" and "nothing here knows where this schema
		// belongs" are different answers: the first is an operator taking a
		// destination back off a schema, whose bundles should move back, and
		// the second is a build of some other command's schema, whose
		// bundles should stay where they are (artifactDirForBuild).
		namespaces[schema] = name
	}
	m.artifactNamespaces = namespaces
	if m.artifactsDir == "" {
		// An external build plane resolves no artifact directory, so there is
		// nothing for a namespace to be under. Nothing is built there either,
		// which is why this is not an error.
		return nil
	}
	// Made now rather than at the first build, so a directory that cannot be
	// created is reported before an event's worth of building rather than in
	// the middle of it.
	for _, name := range namespaces {
		if name == "" {
			continue
		}
		dir := filepath.Join(m.artifactsDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("could not create the artifact directory %s: %w", dir, err)
		}
		// What later tells this directory from any other subdirectory of the
		// artifact directory; see artifactNamespaceMarker.
		marker := filepath.Join(dir, artifactNamespaceMarker)
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return fmt.Errorf("could not mark the artifact directory %s: %w", dir, err)
		}
	}
	return nil
}

// artifactDirFor is where a build of this schema keeps its bundle: the
// namespace its schema was given, or the artifact directory itself.
func (m *Manager) artifactDirFor(schema string) string {
	// No artifact directory at all is an external build plane, which holds no
	// bundles. Guarded rather than assumed: filepath.Join("", "library") is
	// the relative path "library", and a bundle written beside whatever the
	// working directory happens to be would be a hard thing to find.
	if m.artifactsDir == "" {
		return ""
	}
	if name, ok := m.artifactNamespaces[schema]; ok && name != "" {
		return filepath.Join(m.artifactsDir, name)
	}
	return m.artifactsDir
}

// artifactDirForBuild is where this build's bundle goes. When this process
// was told the schema's namespace, that is the answer and nothing else is
// consulted: an operator who has just given a schema a destination means
// its bundles to move there. When it was not, the bundle stays where the
// generation it replaces is -- because a build plane is only ever told the
// namespaces of the schemas one command was given, while an update rebuilds
// every build of a challenge whose source moved, schemas it was not given
// included. Those rebuilds would otherwise land in the artifact directory
// itself and their destination's artifact server would go on publishing the
// generation before, silently.
func (m *Manager) artifactDirForBuild(build *BuildMetadata) string {
	if _, known := m.artifactNamespaces[build.Schema]; known {
		return m.artifactDirFor(build.Schema)
	}
	if where, ok := m.findArtifactBundle(build.getArtifactsFilename()); ok {
		return where
	}
	return m.artifactsDir
}

// findArtifactBundle is the directory under the artifact directory holding
// this bundle, if any: the namespaces first and then the directory itself. A
// build id is unique to a plane, so at most one file can answer to the name.
func (m *Manager) findArtifactBundle(filename string) (string, bool) {
	for _, dir := range m.artifactDirs() {
		if _, err := os.Stat(filepath.Join(dir, filename)); err == nil {
			return dir, true
		}
	}
	return "", false
}

// artifactNamespaceMarker is dropped in every namespace directory cork
// makes, and is what tells one from any other subdirectory of the artifact
// directory. It is needed because the two are not otherwise
// distinguishable and the cost of guessing wrong is a deleted file:
// CMGR_ARTIFACT_DIR is allowed to be the challenge tree (it is in the
// ansible role's defaults), whose subdirectories are challenges, and a
// challenge shipping a file that happened to be named for a build id would
// be swept by a search that took every subdirectory for a namespace.
//
// A dot file, so it is invisible to what publishes the bundles: an artifact
// server takes the "<build id>.tar.gz" files and nothing else.
const artifactNamespaceMarker = ".cork-artifact-namespace"

// artifactDirs is every directory a bundle could be in: the namespaces cork
// made, and the artifact directory itself last, since a plane that has never
// had a namespace keeps everything there.
func (m *Manager) artifactDirs() []string {
	if m.artifactsDir == "" {
		// No artifact directory at all. Returning it anyway would make every
		// lookup below a relative path, and a bundle "found" beside whatever
		// the working directory happens to be is one that would be deleted.
		return nil
	}
	dirs := []string{}
	if entries, err := os.ReadDir(m.artifactsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(m.artifactsDir, entry.Name())
			if _, err := os.Stat(filepath.Join(dir, artifactNamespaceMarker)); err == nil {
				dirs = append(dirs, dir)
			}
		}
	}
	return append(dirs, m.artifactsDir)
}

// pruneStrayArtifactBundle removes a copy of this build's bundle left in the
// artifact directory itself, once the new generation is in place under a
// namespace. There is one only after a schema was given a destination it did
// not have, and leaving it would have an artifact server go on publishing an
// older generation of the same build at the prefix the schema used to use.
//
// Deliberately the artifact directory itself and nowhere else. A filename is
// "<build id>.tar.gz" and a build id is unique only within one plane's
// database: rebuild that database, as BUILDER.md says a plane may, and its
// ids start at 1 again while every destination's directory still holds
// bundles 1..N. A name is therefore not evidence of whose file it is, so
// another destination's directory is never swept -- doing so would delete an
// orchestrator's live bundles on every promote and the artifact server would
// carry that into the bucket. The artifact directory itself is the one place
// a copy can only be this build's own: it is where the schema's bundles were
// written before it was given a destination, which is the case this exists
// for. A destination that CHANGES goes through migrate-schema, which drops
// the rows and removes the bundles outright.
//
// Best-effort: a stray that cannot be removed costs disk and a stale URL, and
// failing a build that has already validated and published would cost more.
func (m *Manager) pruneStrayArtifactBundle(filename string) {
	if m.artifactsDir == "" {
		return
	}
	stray := filepath.Join(m.artifactsDir, filename)
	if err := os.Remove(stray); err == nil {
		m.log.infof("removed %s, left by this build's previous artifact directory", stray)
	} else if !errors.Is(err, os.ErrNotExist) {
		m.log.warnf("could not remove the stray artifact bundle %s: %s", stray, err)
	}
}

// removeArtifactBundle deletes a build's bundle: from the directory its
// schema names, and failing that from anywhere under the artifact directory
// it could be. The search is not belt-and-braces -- it is the only thing that
// works for the commands that do the removing. remove-schema is given a name
// and never learns a destination, and migrate-schema reads the destination a
// schema is moving TO rather than the one its bundles were written under, so
// neither can compute the directory. A build id is unique to a plane, so at
// most one file can answer to the name and there is nothing to disambiguate.
//
// Best-effort past the first directory: a bundle that cannot be removed costs
// disk, and failing a destroy over it would cost the build's row.
func (m *Manager) removeArtifactBundle(schema, filename string) error {
	err := os.Remove(filepath.Join(m.artifactDirFor(schema), filename))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	where, ok := m.findArtifactBundle(filename)
	if !ok {
		return err // the not-exist destroyImages tolerates and logs
	}
	if removeErr := os.Remove(filepath.Join(where, filename)); removeErr != nil {
		return removeErr
	}
	m.log.debugf("removed %s", filepath.Join(where, filename))
	return nil
}

// setChallengeDirectory reads CMGR_DIR, normalizes it to an absolute path
// and requires it to be a directory that exists: a mistyped challenge path
// must fail the start, not bring a daemon up over an empty catalogue.
func (m *Manager) setChallengeDirectory() error {
	chalDir, isSet := os.LookupEnv(DIR_ENV)
	if !isSet {
		chalDir = "."
	}

	var err error
	m.chalDir, err = filepath.Abs(chalDir)
	if err != nil {
		m.log.errorf("could not resolve challenge directory: %s", err)
		return err
	}

	m.log.infof("challenge directory: %s", m.chalDir)

	info, err := os.Stat(m.chalDir)
	if err != nil {
		m.log.errorf("could not stat the challenge directory: %s", err)
		return err
	}

	if !info.IsDir() {
		m.log.error("challenge directory must be a directory")
		return errors.New(m.chalDir + " is not a directory")
	}

	return nil
}

// Performs error checking and calls out to `filepath.Walk` to traverse the directory.
func (m *Manager) inventoryChallenges(dir string) (map[ChallengeId]*ChallengeMetadata, []error) {
	// Crawl the directory
	challenges := make(map[ChallengeId]*ChallengeMetadata)
	errs := []error{}

	m.log.infof("searching %s for challenges", dir)
	err := filepath.Walk(dir, m.findChallenges(&challenges, &errs))
	if err != nil {
		errs = append(errs, err)
		return nil, errs
	}

	return challenges, errs
}

// Wrapper around the function which implements the directory traversal logic.
func (m *Manager) findChallenges(challengeMap *map[ChallengeId]*ChallengeMetadata, errSlice *[]error) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		// Skip errors (adding them to the error list)
		if err != nil {
			*errSlice = append(*errSlice, err)
			return nil
		}

		// Skip files and directories that start with "."
		if info.Name()[0] == '.' {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Don't need to do anything with directories
		if info.IsDir() {
			return nil
		}

		metadata, err := m.loadChallenge(path, info)
		if err != nil {
			*errSlice = append(*errSlice, err)
			return nil
		}

		if metadata == nil {
			return nil
		}

		h := crc32.NewIEEE()
		err = filepath.Walk(filepath.Dir(path), challengeChecksum(filepath.Dir(path), h))
		if err != nil {
			m.log.warnf("could not hash source files: %s", err)
			*errSlice = append(*errSlice, err)
			return nil
		}
		metadata.SourceChecksum = h.Sum32()
		// 0 is reserved: on a build row it means "not built yet"
		// (BuildMetadata.SourceChecksum), so a tree whose CRC happens to be
		// 0 takes 1 instead, as contentChecksum does for its own sentinel.
		if metadata.SourceChecksum == 0 {
			metadata.SourceChecksum = 1
		}

		metadata.Path = path
		m.log.infof("found challenge %s", metadata.Id)

		if val, ok := (*challengeMap)[metadata.Id]; ok {
			err := fmt.Errorf("found multiple challenges with id '%s' at '%s' and '%s'",
				metadata.Id,
				val.Path,
				metadata.Path)
			m.log.error(err)
			return err
		}
		(*challengeMap)[metadata.Id] = metadata

		return nil
	}
}

// Strips the name field down to only alphanumeric runes with dashes.  Strips
// leading and trailing dashes to comply with docker naming conventions.
func sanitizeName(dirty string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9]`)
	return strings.Trim(re.ReplaceAllLiteralString(strings.ToLower(dirty), "-"), "-")
}

func (m *Manager) normalizeDirPath(dir string) (string, error) {
	// Normalize the directory we are passed in
	tgtDir, err := filepath.Abs(dir)
	if err != nil {
		m.log.errorf("bad directory string: %s", err)
		return "", err
	}

	info, err := os.Stat(tgtDir)
	if err != nil {
		m.log.errorf("could not stat directory: %s", err)
		return "", err
	}

	if !info.IsDir() {
		m.log.errorf("expected a directory: %s", tgtDir)
		return "", errors.New(tgtDir + " is not a directory")
	}

	// Check that it is a sub-directory
	if !pathInDirectory(tgtDir, m.chalDir) {
		err := fmt.Errorf("'%s' is not a sub-directory of '%s'", tgtDir, m.chalDir)
		m.log.error(err)
		return "", err
	}

	return tgtDir, nil
}

func pathInDirectory(path, dir string) bool {
	return len(path) >= len(dir) && // Sub-directory cannot be shorter string
		path[:len(dir)] == dir && // Prefix must match
		(len(path) == len(dir) || path[len(dir)] == os.PathSeparator)
}

// The challenge checksum is a checksum of file properties (name, size, mode)
// for all filetypes as well as the actual file contents for non-directories.
// This is a stable checksum because the Go specification for `Walk` promises
// a lexicographical traversal of the directory structure.  Files that start
// with '.' are ignored.
func challengeChecksum(challDir string, h hash.Hash) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		// Consider any error during the walk a fatal problem.
		if err != nil {
			return err
		}

		// Ignore "hidden" files, READMEs, and problem configs
		if checksumIgnore(info.Name()) {

			if info.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		// Add the name, size, and mode fields to the checksum
		_, err = h.Write([]byte(info.Name() +
			fmt.Sprintf("%x", info.Size()) +
			fmt.Sprintf("%x", info.Mode())))
		if err != nil {
			return err
		}

		// If this is not a directory, add the contents to the checksum
		if info.Mode()&os.ModeSymlink == os.ModeSymlink {
			linkTgt, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("Invalid link found at %s: %s", path, err)
			}
			tgt_path := filepath.Join(filepath.Dir(path), linkTgt)

			if !pathInDirectory(tgt_path, challDir) {
				return fmt.Errorf("Encountered symlink at '%s' which points to '%s' which is not in '%s'", path, tgt_path, challDir)
			}

			h.Write([]byte(tgt_path))
		} else if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			_, err = io.Copy(h, f)
			if err != nil {
				return err
			}
		}

		return nil
	}
}

func checksumIgnore(name string) bool {
	return (name[0] == '.' && name != ".dockerignore") ||
		name == "README" ||
		name == "README.md" ||
		name == "problem.md" ||
		name == "solver" ||
		name == "cmgr.db"
}

func contextIgnore(name string) bool {
	return (name[0] == '.' && name != ".dockerignore") ||
		name == "README" ||
		name == "README.md" ||
		name == "problem.md" ||
		name == "solver" ||
		name == "cmgr.db"
}

func (m *Manager) createBuildContext(cm *ChallengeMetadata, dockerfile []byte) (string, error) {
	tmpFile, err := ioutil.TempFile("", "*.tar")
	if err != nil {
		return "", err
	}
	m.log.debug(tmpFile.Name())

	newCtx := tar.NewWriter(tmpFile)
	defer newCtx.Close()

	if dockerfile != nil {
		// Built-in challenge types: the embedded template is pinned on its way
		// in, exactly like a challenge's own Dockerfile below.
		dockerfile = m.pinBases(cm.Id, dockerfile)
		hdr := tar.Header{Name: "Dockerfile", Mode: 0644, Size: int64(len(dockerfile))}

		err = newCtx.WriteHeader(&hdr)
		if err != nil {
			return "", err
		}

		_, err = newCtx.Write(dockerfile)
		if err != nil {
			return "", err
		}
	}

	// Iterate
	challengeDir := filepath.Dir(cm.Path)
	err = filepath.Walk(challengeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		// Ignore "hidden" files, READMEs, and problem configs
		if contextIgnore(info.Name()) || challengeDir == path {

			if info.IsDir() && challengeDir != path {
				return filepath.SkipDir
			}

			return nil
		}

		m.log.debug(path)

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil
		}

		archivePath := path[len(challengeDir)+1:]
		hdr.Name = strings.ReplaceAll(archivePath, `\`, `/`)

		// A custom challenge's own Dockerfile: pin its bases before it enters
		// the context. The content changes, so the header size must be set
		// from the rewritten bytes rather than from the file on disk.
		if !info.IsDir() && hdr.Name == "Dockerfile" {
			original, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			pinned := m.pinBases(cm.Id, original)
			hdr.Size = int64(len(pinned))
			if err = newCtx.WriteHeader(hdr); err != nil {
				return err
			}
			_, err = newCtx.Write(pinned)
			return err
		}

		err = newCtx.WriteHeader(hdr)
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		fd, err := os.Open(path)
		if err != nil {
			return err
		}

		_, err = io.Copy(newCtx, fd)
		if err != nil {
			return err
		}
		fd.Close()

		return nil
	})

	if err != nil {
		return "", err
	}

	return tmpFile.Name(), nil
}
