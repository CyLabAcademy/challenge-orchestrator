// The artifact archive hardening in this file is adapted from
// ArmyCyberInstitute/cmgr at commit bdbf839:
// https://github.com/ArmyCyberInstitute/cmgr
//
// The original project is available under the Apache License 2.0. This file is
// a modified implementation for this fork, also distributed under cmgr's
// Apache License 2.0.
package cmgr

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// artifactLimits returns the effective bounds. The zero fallbacks cover a
// Manager constructed without initPolicy, which happens in tests; they share
// the constants initPolicy defaults to so the two cannot drift.
func (m *Manager) artifactLimits() (int, int64, int64) {
	files := m.policy.MaxArtifactFiles
	totalBytes := m.policy.MaxArtifactBytes
	fileBytes := m.policy.MaxArtifactFileBytes
	if files == 0 {
		files = defaultMaxArtifactFiles
	}
	if totalBytes == 0 {
		totalBytes = defaultMaxArtifactBytes
	}
	if fileBytes == 0 {
		fileBytes = defaultMaxArtifactFileBytes
	}
	return files, totalBytes, fileBytes
}

func safeArchiveName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') ||
		strings.Contains(name, `\`) {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	// Bound the archive-supplied name before cleaning; path.Clean can collapse
	// an arbitrarily long name (e.g. many "./" components) to a short one, so a
	// post-clean check would not enforce this limit on the raw input.
	if len(name) > 4096 {
		return "", fmt.Errorf("archive path is too long: %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || path.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive path escapes its root: %q", name)
	}
	return clean, nil
}

const (
	// artifactMetadataBytesPerEntry allows this many decompressed bytes of tar
	// header and metadata records per permitted entry. A legitimate entry needs
	// a 512-byte header plus, at most, a long-name/PAX record whose name is
	// already bounded to 4096 bytes by safeArchiveName.
	artifactMetadataBytesPerEntry = 8 * 1024
	// artifactTrailerSlackBytes covers the tar end-of-archive blocks and block
	// padding so an archive exactly at its byte limit is not falsely rejected.
	artifactTrailerSlackBytes = 4 * 1024
)

// boundedReader caps the total number of bytes read from an underlying reader
// and reports a clear error once the cap is passed.
type boundedReader struct {
	reader    io.Reader
	remaining int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, fmt.Errorf("artifact archive decompresses beyond its size limit")
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.reader.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (m *Manager) cacheArtifacts(
	source io.Reader,
	destination string,
) (files []string, err error) {
	maxFiles, maxBytes, maxFileBytes := m.artifactLimits()
	tempFile, err := os.CreateTemp(m.artifactsDir, ".cmgr-artifacts-*")
	if err != nil {
		return nil, fmt.Errorf("could not create temporary artifact archive: %w", err)
	}
	tempName := tempFile.Name()
	succeeded := false
	closed := false
	defer func() {
		if !closed {
			if closeErr := tempFile.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}
		if !succeeded {
			_ = os.Remove(tempName)
		}
	}()
	if err := tempFile.Chmod(0600); err != nil {
		return nil, err
	}

	sourceGzip, err := gzip.NewReader(source)
	if err != nil {
		return nil, fmt.Errorf("could not decode artifact gzip stream: %w", err)
	}
	defer sourceGzip.Close()
	// tar.Reader.Next consumes PAX, global, and GNU long-name records
	// internally before returning a file header, so the per-entry and per-total
	// checks below never see them. Bound the decompressed stream as a whole so a
	// flood of highly compressible metadata records cannot force unbounded
	// decompression: the cap is the allowed body bytes plus a header/metadata
	// allowance scaled to the entry limit, plus slack for the tar trailer.
	decompressedLimit := maxBytes +
		int64(maxFiles)*artifactMetadataBytesPerEntry +
		artifactTrailerSlackBytes
	sourceTar := tar.NewReader(&boundedReader{reader: sourceGzip, remaining: decompressedLimit})
	destinationGzip := gzip.NewWriter(tempFile)
	destinationTar := tar.NewWriter(destinationGzip)

	seen := make(map[string]struct{})
	var totalBytes int64
	entryCount := 0
	for {
		header, nextErr := sourceTar.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, fmt.Errorf("could not read artifact archive: %w", nextErr)
		}
		entryCount++
		if entryCount > maxFiles {
			return nil, fmt.Errorf(
				"artifact archive contains more than %d entries",
				maxFiles,
			)
		}
		// `tar -C dir .` emits a literal "./" (or ".") root entry that carries
		// no content. Skip it rather than failing an otherwise legitimate
		// archive. It is counted above so a flood of root entries cannot evade
		// maxFiles, and the match is restricted to the conventional root names
		// so traversal-style names like "a/.." still reach safeArchiveName.
		if header.Typeflag == tar.TypeDir &&
			(header.Name == "." || header.Name == "./") {
			continue
		}
		name, err := safeArchiveName(header.Name)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("artifact archive repeats path %q", name)
		}
		seen[name] = struct{}{}

		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxFileBytes {
				return nil, fmt.Errorf(
					"artifact %q is %d bytes; per-file maximum is %d",
					name,
					header.Size,
					maxFileBytes,
				)
			}
			if header.Size > maxBytes-totalBytes {
				return nil, fmt.Errorf(
					"artifact archive exceeds %d uncompressed bytes",
					maxBytes,
				)
			}
			totalBytes += header.Size
			files = append(files, name)
		case tar.TypeDir:
			header.Size = 0
		default:
			return nil, fmt.Errorf(
				"artifact %q has unsupported tar type %d",
				name,
				header.Typeflag,
			)
		}

		cleanHeader := *header
		cleanHeader.Name = name
		cleanHeader.Linkname = ""
		cleanHeader.Mode &= 0777
		cleanHeader.Uid = 0
		cleanHeader.Gid = 0
		cleanHeader.Uname = ""
		cleanHeader.Gname = ""
		cleanHeader.PAXRecords = nil
		cleanHeader.Xattrs = nil
		if err := destinationTar.WriteHeader(&cleanHeader); err != nil {
			return nil, fmt.Errorf("could not write artifact header: %w", err)
		}
		if cleanHeader.Typeflag == tar.TypeReg ||
			cleanHeader.Typeflag == tar.TypeRegA {
			if _, err := io.CopyN(destinationTar, sourceTar, cleanHeader.Size); err != nil {
				return nil, fmt.Errorf("could not copy artifact %q: %w", name, err)
			}
		}
	}
	if err := sourceGzip.Close(); err != nil {
		return nil, fmt.Errorf("could not close artifact gzip stream: %w", err)
	}
	if err := destinationTar.Close(); err != nil {
		return nil, fmt.Errorf("could not finish artifact tar stream: %w", err)
	}
	if err := destinationGzip.Close(); err != nil {
		return nil, fmt.Errorf("could not finish artifact gzip stream: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		return nil, fmt.Errorf("could not sync artifact archive: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return nil, fmt.Errorf("could not close artifact archive: %w", err)
	}
	closed = true
	if err := os.Rename(tempName, destination); err != nil {
		return nil, fmt.Errorf("could not install artifact archive: %w", err)
	}
	if directory, err := os.Open(filepath.Dir(destination)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	succeeded = true
	return files, nil
}
