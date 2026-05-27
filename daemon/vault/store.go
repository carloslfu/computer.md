// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Secret is a single encrypted secret entry.
type Secret struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"` // only populated when decrypted in memory
	Label     string    `json:"label"` // human-readable label
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SecretInfo is the public metadata for a secret (no value exposed).
type SecretInfo struct {
	Name      string    `json:"name"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store manages encrypted secrets using AES-256-GCM.
// Secrets are stored as an encrypted JSON blob on disk.
type Store struct {
	mu       sync.RWMutex
	path     string
	key      [32]byte
	secrets  map[string]*Secret
}

// NewStore creates or loads a vault from the given file path.
// The key must be exactly 32 bytes for AES-256.
func NewStore(path string, key [32]byte) (*Store, error) {
	s := &Store{
		path:    path,
		key:     key,
		secrets: make(map[string]*Secret),
	}

	// Load existing vault if present.
	if _, err := os.Stat(path); err == nil {
		if err := s.load(); err != nil {
			return nil, fmt.Errorf("loading vault: %w", err)
		}
	}

	return s, nil
}

// Set creates or updates a secret.
func (s *Store) Set(name, value, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()

	existing, exists := s.secrets[name]
	if exists {
		existing.Value = value
		existing.Label = label
		existing.UpdatedAt = now
	} else {
		s.secrets[name] = &Secret{
			Name:      name,
			Value:     value,
			Label:     label,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	return s.save()
}

// Get retrieves a secret value by name. Returns empty string if not found.
func (s *Store) Get(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	secret, ok := s.secrets[name]
	if !ok {
		return "", false
	}
	return secret.Value, true
}

// Delete removes a secret by name.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.secrets[name]; !ok {
		return fmt.Errorf("secret not found: %s", name)
	}

	delete(s.secrets, name)
	return s.save()
}

// List returns metadata for all secrets (no values exposed).
func (s *Store) List() []SecretInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var infos []SecretInfo
	for _, secret := range s.secrets {
		infos = append(infos, SecretInfo{
			Name:      secret.Name,
			Label:     secret.Label,
			CreatedAt: secret.CreatedAt,
			UpdatedAt: secret.UpdatedAt,
		})
	}
	return infos
}

// Names returns all secret names.
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var names []string
	for name := range s.secrets {
		names = append(names, name)
	}
	return names
}

// AllValues returns all secret values (used internally for masking).
func (s *Store) AllValues() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	values := make(map[string]string)
	for name, secret := range s.secrets {
		values[name] = secret.Value
	}
	return values
}

// ResolveReferences replaces $SECRET_NAME patterns in text with actual values.
func (s *Store) ResolveReferences(text string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := text
	for name, secret := range s.secrets {
		placeholder := "$" + name
		result = strings.ReplaceAll(result, placeholder, secret.Value)
		// Also support ${SECRET_NAME} syntax.
		placeholder = "${" + name + "}"
		result = strings.ReplaceAll(result, placeholder, secret.Value)
	}
	return result
}

// BuildEnv builds a KEY=VALUE environment slice for the given secret names.
// Names absent from the vault are skipped (no entry emitted). Repeated names
// are deduplicated, first occurrence wins. This is the only place a plaintext
// secret value is copied out of the store for delivery to a child process —
// the returned slice is handed directly to cmd.Env, never into argv, so the
// secret never appears in ps/​/proc/<pid>/cmdline.
func (s *Store) BuildEnv(names []string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	env := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		secret, ok := s.secrets[name]
		if !ok {
			continue
		}
		seen[name] = true
		env = append(env, name+"="+secret.Value)
	}
	return env
}

// save encrypts and writes all secrets to disk.
func (s *Store) save() error {
	data, err := json.Marshal(s.secrets)
	if err != nil {
		return fmt.Errorf("marshaling secrets: %w", err)
	}

	encrypted, err := s.encrypt(data)
	if err != nil {
		return fmt.Errorf("encrypting vault: %w", err)
	}

	// Write atomically via temp file.
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, encrypted, 0600); err != nil {
		return fmt.Errorf("writing vault: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming vault: %w", err)
	}

	return nil
}

// load decrypts and reads all secrets from disk.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("reading vault file: %w", err)
	}

	if len(data) == 0 {
		return nil
	}

	decrypted, err := s.decrypt(data)
	if err != nil {
		return fmt.Errorf("decrypting vault: %w", err)
	}

	if err := json.Unmarshal(decrypted, &s.secrets); err != nil {
		return fmt.Errorf("parsing vault data: %w", err)
	}

	return nil
}

// encrypt uses AES-256-GCM to encrypt plaintext.
func (s *Store) encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt uses AES-256-GCM to decrypt ciphertext.
func (s *Store) decrypt(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}
