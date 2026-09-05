// The artifact resource bounds in this file are adapted from
// ArmyCyberInstitute/cmgr at commit bdbf839, which introduced configurable
// limits alongside its artifact hardening work:
// https://github.com/ArmyCyberInstitute/cmgr
//
// The original project is available under the Apache License 2.0. This file is
// a reduced implementation covering only the artifact limits, also distributed
// under cmgr's Apache License 2.0.
package cmgr

import (
	"fmt"
	"os"
	"strconv"

	"github.com/docker/go-units"
)

const (
	maxArtifactFilesEnv     = "CMGR_MAX_ARTIFACT_FILES"
	maxArtifactBytesEnv     = "CMGR_MAX_ARTIFACT_BYTES"
	maxArtifactFileBytesEnv = "CMGR_MAX_ARTIFACT_FILE_BYTES"
)

// Defaults for the artifact bounds. artifactLimits falls back to these too, so
// they are declared once here.
const (
	defaultMaxArtifactFiles     = 10_000
	defaultMaxArtifactBytes     = 5 << 30
	defaultMaxArtifactFileBytes = 1 << 30
)

// managerPolicy holds deployment-wide resource bounds. Upstream's equivalent
// also carries container runtime defaults, build concurrency, and solver
// timeouts; those are deliberately not adopted here because this fork already
// manages them elsewhere.
type managerPolicy struct {
	MaxArtifactFiles     int
	MaxArtifactBytes     int64
	MaxArtifactFileBytes int64
}

func envString(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func positiveEnvInt(name string, fallback int) (int, error) {
	value := envString(name, strconv.Itoa(fallback))
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, value)
	}
	return parsed, nil
}

func positiveEnvBytes(name string, fallback int64) (int64, error) {
	value, isSet := os.LookupEnv(name)
	if !isSet {
		return fallback, nil
	}
	parsed, err := units.RAMInBytes(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive byte size, got %q", name, value)
	}
	return parsed, nil
}

func (m *Manager) initPolicy() error {
	var err error
	if m.policy.MaxArtifactFiles, err = positiveEnvInt(
		maxArtifactFilesEnv, defaultMaxArtifactFiles); err != nil {
		return err
	}
	if m.policy.MaxArtifactBytes, err = positiveEnvBytes(
		maxArtifactBytesEnv, defaultMaxArtifactBytes); err != nil {
		return err
	}
	if m.policy.MaxArtifactFileBytes, err = positiveEnvBytes(
		maxArtifactFileBytesEnv, defaultMaxArtifactFileBytes); err != nil {
		return err
	}
	return nil
}
