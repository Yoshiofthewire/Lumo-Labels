package proton

import (
	"os"
	"path/filepath"
	"testing"

	protonapi "github.com/ProtonMail/go-proton-api"
)

func setTokenFileEnv(t *testing.T, path string) {
	t.Helper()
	prev, had := os.LookupEnv("PROTON_AUTH_FILE")
	if err := os.Setenv("PROTON_AUTH_FILE", path); err != nil {
		t.Fatalf("set PROTON_AUTH_FILE: %v", err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("PROTON_AUTH_FILE", prev)
		} else {
			_ = os.Unsetenv("PROTON_AUTH_FILE")
		}
	})
}

func TestReadTokenFileWithSourceFallsBackToSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proton-auth.json")
	setTokenFileEnv(t, path)

	// Main file is malformed; snapshot is a valid last-known-good copy.
	if err := os.WriteFile(path, []byte("{ this is not valid json"), 0o600); err != nil {
		t.Fatalf("write main: %v", err)
	}
	snapshot := `{"uid":"u1","accessToken":"a1","refreshToken":"r1"}`
	if err := os.WriteFile(tokenSnapshotFilePath(path), []byte(snapshot), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	uid, acc, ref, source, err := readTokenFileWithSource()
	if err != nil {
		t.Fatalf("expected fallback success, got error: %v", err)
	}
	if source != "snapshot" {
		t.Fatalf("expected source=snapshot, got %q", source)
	}
	if uid != "u1" || acc != "a1" || ref != "r1" {
		t.Fatalf("unexpected tokens: uid=%q acc=%q ref=%q", uid, acc, ref)
	}
}

func TestUpdateTokenFileWritesAtomicallyAndMaintainsSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proton-auth.json")
	setTokenFileEnv(t, path)

	if err := writeTokenFile("u1", "a1", "r1"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// A second rotation must overwrite cleanly and refresh the snapshot.
	if err := writeTokenFile("u1", "a2", "r2"); err != nil {
		t.Fatalf("second write: %v", err)
	}

	uid, acc, ref, err := readTokenFileFromPath(path)
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	if uid != "u1" || acc != "a2" || ref != "r2" {
		t.Fatalf("main not updated: uid=%q acc=%q ref=%q", uid, acc, ref)
	}

	suid, sacc, sref, err := readTokenFileFromPath(tokenSnapshotFilePath(path))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if suid != "u1" || sacc != "a2" || sref != "r2" {
		t.Fatalf("snapshot not updated: uid=%q acc=%q ref=%q", suid, sacc, sref)
	}

	// No stray temp files should remain after atomic renames.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("unexpected leftover temp file: %s", e.Name())
		}
	}
}

func TestPersistRotatedAuthRecordsSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proton-auth.json")
	setTokenFileEnv(t, path)

	c := &APIClient{}
	err := c.persistRotatedAuth(protonapi.Auth{UID: "u1", AccessToken: "a1", RefreshToken: "r1"})
	if err != nil {
		t.Fatalf("persist should succeed: %v", err)
	}

	info := c.DebugAuthState()
	if info.LastPersistErr != "" {
		t.Fatalf("expected no persist error, got %q", info.LastPersistErr)
	}
	if info.LastPersistAt == "" {
		t.Fatalf("expected lastPersistAt to be recorded")
	}

	uid, acc, ref, err := readTokenFileFromPath(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if uid != "u1" || acc != "a1" || ref != "r1" {
		t.Fatalf("unexpected persisted tokens: uid=%q acc=%q ref=%q", uid, acc, ref)
	}
}

func TestPersistRotatedAuthRecordsFailure(t *testing.T) {
	dir := t.TempDir()
	// Point the auth file inside a path whose parent is a regular file, so
	// MkdirAll fails and the token write returns an error.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	setTokenFileEnv(t, filepath.Join(blocker, "proton-auth.json"))

	c := &APIClient{}
	err := c.persistRotatedAuth(protonapi.Auth{UID: "u1", AccessToken: "a1", RefreshToken: "r1"})
	if err == nil {
		t.Fatalf("expected persist failure when directory cannot be created")
	}

	info := c.DebugAuthState()
	if info.LastPersistErr == "" {
		t.Fatalf("expected lastPersistError to be recorded on failure")
	}
}
