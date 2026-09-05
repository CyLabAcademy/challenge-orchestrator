package cmgr

import (
	"os"
	"path/filepath"
	"testing"
)

// The artifact directory and the database's directory are created at
// startup: on a fresh box only the challenge tree has to exist already.
func TestSetDirectoriesCreatesArtifactsDir(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	root := t.TempDir()
	artifacts := filepath.Join(root, "var", "lib", "cork", "artifacts")
	t.Setenv(DIR_ENV, root)
	t.Setenv(ARTIFACT_DIR_ENV, artifacts)

	if err := m.setDirectories(); err != nil {
		t.Fatalf("setDirectories: %s", err)
	}
	if info, err := os.Stat(artifacts); err != nil || !info.IsDir() {
		t.Fatalf("artifacts directory was not created: %v", err)
	}
}

func TestSetDirectoriesRequiresChallengeDir(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	t.Setenv(DIR_ENV, filepath.Join(t.TempDir(), "missing"))
	t.Setenv(ARTIFACT_DIR_ENV, t.TempDir())
	if err := m.setDirectories(); err == nil {
		t.Fatal("a missing challenge directory was accepted")
	}
}

func TestInitDatabaseCreatesDirectory(t *testing.T) {
	m := &Manager{log: newLogger(DISABLED)}
	dbPath := filepath.Join(t.TempDir(), "var", "lib", "cork", "cmgr.db")
	t.Setenv(DB_ENV, dbPath)

	if err := m.initDatabase(); err != nil {
		t.Fatalf("initDatabase: %s", err)
	}
	defer m.db.Close()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file was not created: %s", err)
	}
}
