package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// Fake store for abstraction tests.

type fakeStore struct {
	creds   *Credentials
	loadErr error
	saveErr error
	delErr  error
	saved   *Credentials
	deleted bool
}

func (f *fakeStore) load() (*Credentials, error) {
	return f.creds, f.loadErr
}

func (f *fakeStore) save(c *Credentials) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = c
	return nil
}

func (f *fakeStore) delete() error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = true
	return nil
}

func withFakes(secure, file *fakeStore, fn func()) {
	origSecure := newSecureStoreFn
	origFile := newFileStoreFn
	newSecureStoreFn = func() credentialStore { return secure }
	newFileStoreFn = func() credentialStore { return file }
	defer func() {
		newSecureStoreFn = origSecure
		newFileStoreFn = origFile
	}()
	fn()
}

var testCreds = &Credentials{
	AccessToken:  "at",
	RefreshToken: "rt",
	ExpiresAt:    9999999999,
	Email:        "test@example.com",
	Endpoint:     "https://api.semantica.sh",
}

// Store abstraction tests.

func TestLoadCredentials_SecureStore(t *testing.T) {
	secure := &fakeStore{creds: testCreds}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		creds, err := LoadCredentials()
		if err != nil {
			t.Fatal(err)
		}
		if creds == nil || creds.Email != "test@example.com" {
			t.Errorf("expected creds from secure store, got %+v", creds)
		}
	})
}

func TestLoadCredentials_FileFallback(t *testing.T) {
	secure := &fakeStore{loadErr: fmt.Errorf("dbus not running")}
	file := &fakeStore{creds: testCreds}

	withFakes(secure, file, func() {
		creds, err := LoadCredentials()
		if err != nil {
			t.Fatal(err)
		}
		if creds == nil || creds.Email != "test@example.com" {
			t.Errorf("expected creds from file fallback, got %+v", creds)
		}
		if secure.saved != nil {
			t.Error("should not migrate when secure store is unavailable")
		}
	})
}

func TestLoadCredentials_MigrateFileToSecure(t *testing.T) {
	secure := &fakeStore{loadErr: keyring.ErrNotFound}
	file := &fakeStore{creds: testCreds}

	withFakes(secure, file, func() {
		creds, err := LoadCredentials()
		if err != nil {
			t.Fatal(err)
		}
		if creds == nil || creds.Email != "test@example.com" {
			t.Errorf("expected creds, got %+v", creds)
		}
		if secure.saved == nil {
			t.Error("expected migration to secure store")
		}
		if !file.deleted {
			t.Error("expected file deletion after migration")
		}
	})
}

func TestLoadCredentials_MigrationFailure_StillReturns(t *testing.T) {
	secure := &fakeStore{
		loadErr: keyring.ErrNotFound,
		saveErr: fmt.Errorf("keyring write denied"),
	}
	file := &fakeStore{creds: testCreds}

	withFakes(secure, file, func() {
		creds, err := LoadCredentials()
		if err != nil {
			t.Fatal(err)
		}
		if creds == nil || creds.Email != "test@example.com" {
			t.Errorf("expected creds despite migration failure, got %+v", creds)
		}
		if file.deleted {
			t.Error("should not delete file when migration fails")
		}
	})
}

func TestLoadCredentials_NeitherHasCredentials(t *testing.T) {
	secure := &fakeStore{loadErr: keyring.ErrNotFound}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		creds, err := LoadCredentials()
		if err != nil {
			t.Fatal(err)
		}
		if creds != nil {
			t.Errorf("expected nil, got %+v", creds)
		}
	})
}

func TestSaveCredentials_SecureStore(t *testing.T) {
	secure := &fakeStore{}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		if err := SaveCredentials(testCreds); err != nil {
			t.Fatal(err)
		}
		if secure.saved == nil {
			t.Error("expected save to secure store")
		}
		if !file.deleted {
			t.Error("expected old file deleted after secure save")
		}
	})
}

func TestSaveCredentials_FileFallback_KeyringUnavailable(t *testing.T) {
	// A D-Bus error means the keyring service is unavailable.
	secure := &fakeStore{
		saveErr: fmt.Errorf("failed to connect to dbus session bus"),
	}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		if err := SaveCredentials(testCreds); err != nil {
			t.Fatal(err)
		}
		if file.saved == nil {
			t.Error("expected save to file when keyring is unavailable")
		}
	})
}

func TestSaveCredentials_KeyringAvailableButSaveDenied(t *testing.T) {
	// An access error on a reachable keyring must not fall back to file.
	secure := &fakeStore{
		saveErr: fmt.Errorf("access denied by keychain"),
	}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		err := SaveCredentials(testCreds)
		if err == nil {
			t.Fatal("expected error when keyring is available but save denied")
		}
		if file.saved != nil {
			t.Error("should NOT fall back to file when keyring is reachable")
		}
	})
}

func TestSaveCredentials_KeyringLockedNotFallback(t *testing.T) {
	// Locked keychain is a real error, not unavailability.
	secure := &fakeStore{
		saveErr: fmt.Errorf("keychain is locked"),
	}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		err := SaveCredentials(testCreds)
		if err == nil {
			t.Fatal("expected error when keychain is locked")
		}
		if file.saved != nil {
			t.Error("should NOT fall back to file when keychain is locked")
		}
	})
}

func TestSaveCredentials_FileCleanupError_Surfaced(t *testing.T) {
	secure := &fakeStore{}
	file := &fakeStore{delErr: fmt.Errorf("permission denied")}

	withFakes(secure, file, func() {
		err := SaveCredentials(testCreds)
		if err == nil {
			t.Fatal("expected error when old file can't be deleted")
		}
		if secure.saved == nil {
			t.Error("credentials should still be saved to secure store")
		}
	})
}

func TestDeleteCredentials_Both(t *testing.T) {
	secure := &fakeStore{}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		if err := DeleteCredentials(); err != nil {
			t.Fatal(err)
		}
		if !secure.deleted {
			t.Error("expected secure store deletion")
		}
		if !file.deleted {
			t.Error("expected file deletion")
		}
	})
}

func TestDeleteCredentials_NeitherExists(t *testing.T) {
	secure := &fakeStore{}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		if err := DeleteCredentials(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeleteCredentials_KeyringError_Surfaced(t *testing.T) {
	secure := &fakeStore{delErr: fmt.Errorf("access denied")}
	file := &fakeStore{}

	withFakes(secure, file, func() {
		err := DeleteCredentials()
		if err == nil {
			t.Fatal("expected error when keyring delete fails")
		}
		if !file.deleted {
			t.Error("file should still be deleted even when keyring fails")
		}
	})
}

func TestDeleteCredentials_FileError_Surfaced(t *testing.T) {
	secure := &fakeStore{}
	file := &fakeStore{delErr: fmt.Errorf("permission denied")}

	withFakes(secure, file, func() {
		err := DeleteCredentials()
		if err == nil {
			t.Fatal("expected error when file delete fails")
		}
		if !secure.deleted {
			t.Error("secure store should still be deleted even when file fails")
		}
	})
}

// File store integration tests (using real file I/O).

func TestFileStore_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	disableSecureStore(t)

	original := &Credentials{
		AccessToken:  "at-abc123",
		RefreshToken: "rt-def456",
		ExpiresAt:    time.Now().Unix() + 3600,
		Email:        "test@example.com",
		Endpoint:     "https://api.semantica.sh",
	}

	if err := SaveCredentials(original); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil credentials")
	}
	if loaded.AccessToken != original.AccessToken {
		t.Errorf("access_token = %q, want %q", loaded.AccessToken, original.AccessToken)
	}
	if loaded.Email != original.Email {
		t.Errorf("email = %q, want %q", loaded.Email, original.Email)
	}
}

func TestFileStore_DeleteRemovesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	disableSecureStore(t)

	if err := SaveCredentials(&Credentials{
		AccessToken: "at-xyz",
		ExpiresAt:   time.Now().Unix() + 3600,
	}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "semantica", "credentials.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}

	if err := DeleteCredentials(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected file to be deleted")
	}
}

// Utility tests.

func TestIsExpired(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt int64
		want      bool
	}{
		{"expired", time.Now().Unix() - 60, true},
		{"just expired", time.Now().Unix(), true},
		{"valid", time.Now().Unix() + 3600, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Credentials{ExpiresAt: tt.expiresAt}
			if got := c.IsExpired(); got != tt.want {
				t.Errorf("IsExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigDir_XDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	got, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dir, "semantica")
	if got != want {
		t.Errorf("ConfigDir() = %q, want %q", got, want)
	}
}

func TestConfigDir_Default(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")

	got, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}

	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config", "semantica")
	if got != want {
		t.Errorf("ConfigDir() = %q, want %q", got, want)
	}
}
