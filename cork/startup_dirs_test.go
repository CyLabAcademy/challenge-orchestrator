package cork

import (
	"os"
	"path/filepath"
	"strings"
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

// A symlinked challenge directory is refused rather than followed. os.Stat
// resolves the link and would accept it; filepath.Walk lstats its root and
// descends into nothing, producing an empty inventory with no error -- which
// classifies every challenge on record as Removed and drops it.
func TestSetDirectoriesRefusesSymlinkedChallengeDir(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatalf("making the tree: %s", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(tree, link); err != nil {
		t.Skipf("symlinks cannot be created here: %s", err)
	}

	m := &Manager{log: newLogger(DISABLED)}
	t.Setenv(DIR_ENV, link)
	t.Setenv(ARTIFACT_DIR_ENV, t.TempDir())

	err := m.setDirectories()
	if err == nil {
		t.Fatal("a symlinked challenge directory was accepted: the scan would find nothing and take every challenge on record to have been removed")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the refusal does not say what is wrong: %s", err)
	}

	// The directory the link points at is accepted, so the refusal is about
	// the link and not about the tree underneath it.
	t.Setenv(DIR_ENV, tree)
	if err := m.setDirectories(); err != nil {
		t.Fatalf("the directory the link pointed at was refused too: %s", err)
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
