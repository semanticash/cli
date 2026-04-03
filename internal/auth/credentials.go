package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/util"
	"github.com/zalando/go-keyring"
)

const (
	keyringService = "semantica"
	keyringUser    = "credentials"
)

// Credentials holds the user's authentication tokens.
type Credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix seconds
	Email        string `json:"email,omitempty"`
	Endpoint     string `json:"endpoint"`
}

// IsExpired returns true if the access token has expired.
func (c *Credentials) IsExpired() bool {
	return time.Now().Unix() >= c.ExpiresAt
}

// --- Storage abstraction ---

// credentialStore abstracts credential storage backends.
type credentialStore interface {
	load() (*Credentials, error) // returns (nil, ErrNotFound-equivalent) if empty
	save(c *Credentials) error
	delete() error
}

// Test hooks for storage backends.
var (
	newSecureStoreFn = func() credentialStore { return &secureStore{} }
	newFileStoreFn   = func() credentialStore { return &fileStore{} }
)

// --- Public API (unchanged signatures) ---

// LoadCredentials reads credentials from secure storage first, then falls
// back to file storage. It also migrates file-backed credentials into secure
// storage when possible.
// Returns (nil, nil) if no credentials exist in either store.
func LoadCredentials() (*Credentials, error) {
	secure := newSecureStoreFn()
	file := newFileStoreFn()

	// Try secure store first.
	creds, secureErr := secure.load()
	if secureErr == nil && creds != nil {
		return creds, nil
	}
	// Treat ErrNotFound as an available but empty keyring.
	secureAvailable := secureErr == nil || errors.Is(secureErr, keyring.ErrNotFound)

	// Fall back to file.
	creds, fileErr := file.load()
	if fileErr != nil {
		return nil, fileErr
	}
	if creds == nil {
		return nil, nil
	}

	// Migrate file-backed credentials into secure storage when the keyring works.
	if secureAvailable {
		if migrateErr := secure.save(creds); migrateErr == nil {
			_ = file.delete()
		}
	}

	return creds, nil
}

// SaveCredentials writes credentials to secure storage first, falling back
// to file storage only if the keyring service is genuinely unavailable.
// If the keyring is reachable but save fails (locked, access denied), the
// error is returned - credentials are not silently downgraded to plaintext.
func SaveCredentials(c *Credentials) error {
	secure := newSecureStoreFn()
	file := newFileStoreFn()

	saveErr := secure.save(c)
	if saveErr == nil {
		// Saved to secure store. Clean up any old plaintext file.
		if delErr := file.delete(); delErr != nil {
			return fmt.Errorf("saved to secure storage but failed to remove old credentials file: %w", delErr)
		}
		return nil
	}

	// Save failed. Only fall back to file if the keyring service is
	// genuinely unavailable. On macOS, Keychain is always present so
	// any error is a real failure. On Linux, a missing D-Bus/keyring
	// daemon produces specific errors we can detect.
	if isKeyringUnavailable(saveErr) {
		return file.save(c)
	}

	// Keyring is reachable but save failed - do not downgrade to file.
	return fmt.Errorf("save to secure storage failed: %w", saveErr)
}

// isKeyringUnavailable returns true if the error indicates the keyring
// service itself is not running, as opposed to a permission or access error
// on a running service. This is the only case where file fallback is safe.
func isKeyringUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// go-keyring on Linux without a running Secret Service / D-Bus daemon.
	for _, pattern := range []string{
		"secret service",
		"dbus",
		"org.freedesktop.secrets",
		"exec:",
		"not found in PATH",
	} {
		if strings.Contains(strings.ToLower(msg), pattern) {
			return true
		}
	}
	return false
}

// DeleteCredentials removes credentials from both secure and file storage.
// Ignores "not found" from either store. Ignores keyring errors when the
// keyring service is genuinely unavailable (headless/CI) - in that case
// file deletion alone is sufficient.
func DeleteCredentials() error {
	secure := newSecureStoreFn()
	file := newFileStoreFn()

	secureErr := secure.delete()
	fileErr := file.delete()

	var errs []string
	if secureErr != nil && !isKeyringUnavailable(secureErr) {
		errs = append(errs, fmt.Sprintf("keyring: %v", secureErr))
	}
	if fileErr != nil {
		errs = append(errs, fmt.Sprintf("file: %v", fileErr))
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete credentials: %s", strings.Join(errs, "; "))
	}
	return nil
}

// --- Secure store (OS keychain/keyring) ---

type secureStore struct{}

func (s *secureStore) load() (*Credentials, error) {
	secret, err := keyring.Get(keyringService, keyringUser)
	if err != nil {
		return nil, err // includes ErrNotFound; caller handles it
	}
	var c Credentials
	if err := json.Unmarshal([]byte(secret), &c); err != nil {
		return nil, fmt.Errorf("parse keyring credentials: %w", err)
	}
	return &c, nil
}

func (s *secureStore) save(c *Credentials) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return keyring.Set(keyringService, keyringUser, string(data))
}

func (s *secureStore) delete() error {
	err := keyring.Delete(keyringService, keyringUser)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// --- File store (fallback) ---

type fileStore struct{}

func (f *fileStore) load() (*Credentials, error) {
	path, err := CredentialsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	return &c, nil
}

func (f *fileStore) save(c *Credentials) error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename credentials: %w", err)
	}
	return nil
}

func (f *fileStore) delete() error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete credentials: %w", err)
	}
	return nil
}

// --- Helpers ---

// ConfigDir returns the semantica config directory.
func ConfigDir() (string, error) {
	return util.AppConfigDir()
}

// CredentialsPath returns the full path to the credentials file.
func CredentialsPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}
