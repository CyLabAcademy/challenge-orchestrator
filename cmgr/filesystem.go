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
		m.noteIgnoredSetting(DIR_ENV)
	} else if err = m.setChallengeDirectory(); err != nil {
		return err
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
