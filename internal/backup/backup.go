// Package backup writes and restores an encrypted backup of a node: its
// identity key and certificate, and a consistent snapshot of its database,
// which holds the group log, publications, opt-outs, blocks, invitations,
// sync state, and the audit log (design-spec section 13).
//
// Configuration is not included. It is supplied by the deployment's
// environment, such as its Compose file, and is backed up with that. The one
// secret it holds, the Jellyfin API key, can be reissued from Jellyfin.
//
// A backup is encrypted with AES-256-GCM under a key derived from a
// passphrase by PBKDF2-SHA256. The header (format, salt, iteration count) is
// authenticated as additional data, so it cannot be altered to weaken the
// derivation without the restore failing.
package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"jellymesh/internal/node"
	"jellymesh/internal/store"
)

const (
	magic = "JMBACKUP"
	// formatVersion 1: magic, version byte, 16-byte salt, uint32 iterations,
	// 12-byte nonce, then the sealed tar archive.
	formatVersion = 1

	// Iterations follows current guidance for PBKDF2-HMAC-SHA256.
	Iterations = 600_000

	// minimumIterations refuses a header that was edited to make the
	// passphrase cheap to attack; authentication would catch the edit, but
	// the derivation runs before authentication can.
	minimumIterations = 100_000

	saltSize  = 16
	nonceSize = 12

	// maxArchive bounds what a restore will decrypt into memory.
	maxArchive = 1 << 30

	// MinimumPassphrase is the shortest passphrase accepted for a new
	// backup. The key file is in the backup, so the passphrase protects the
	// node's identity.
	MinimumPassphrase = 12

	manifestName = "manifest.json"
	databaseName = "jellymesh.db"
	keyName      = "node.key"
	certName     = "node.crt"
)

var (
	ErrWrongPassphrase = errors.New("the passphrase is wrong or the backup has been altered")
	ErrNotABackup      = errors.New("the file is not a Jellymesh backup")
	ErrWeakPassphrase  = fmt.Errorf("a backup passphrase must be at least %d characters", MinimumPassphrase)
	ErrDestinationUsed = errors.New("refusing to restore over an existing node")
	ErrInconsistent    = errors.New("the backup's parts do not belong to the same node")
)

// Manifest describes a backup.
type Manifest struct {
	Version       int               `json:"version"`
	CreatedAt     time.Time         `json:"created_at"`
	Fingerprint   node.Fingerprint  `json:"fingerprint"`
	SchemaVersion int               `json:"schema_version"`
	Digests       map[string]string `json:"digests"`
}

// Paths locates a node's state on disk.
type Paths struct {
	Database string
	Key      string
	Cert     string
}

// Create writes an encrypted backup of the node to out.
func Create(ctx context.Context, database *store.DB, identity *node.Identity, paths Paths, passphrase string, out io.Writer) error {
	if len(passphrase) < MinimumPassphrase {
		return ErrWeakPassphrase
	}
	// Stage beside the database, whose directory is already owner-only,
	// rather than in a shared temporary directory.
	staging, err := os.MkdirTemp(filepath.Dir(paths.Database), ".backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}

	snapshot := filepath.Join(staging, databaseName)
	if err := database.Snapshot(ctx, snapshot); err != nil {
		return err
	}
	parts := map[string][]byte{}
	for name, path := range map[string]string{databaseName: snapshot, keyName: paths.Key, certName: paths.Cert} {
		if parts[name], err = os.ReadFile(path); err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
	}
	schema, err := database.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	manifest := Manifest{
		Version: formatVersion, CreatedAt: time.Now().UTC(), Fingerprint: identity.Fingerprint(),
		SchemaVersion: schema, Digests: map[string]string{},
	}
	for name, data := range parts {
		sum := sha256.Sum256(data)
		manifest.Digests[name] = fmt.Sprintf("%x", sum)
	}
	encodedManifest, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, name := range []string{manifestName, databaseName, keyName, certName} {
		data := parts[name]
		if name == manifestName {
			data = encodedManifest
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: manifest.CreatedAt}); err != nil {
			return err
		}
		if _, err := writer.Write(data); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return seal(passphrase, archive.Bytes(), out)
}

func seal(passphrase string, plaintext []byte, out io.Writer) error {
	salt := make([]byte, saltSize)
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	header := encodeHeader(salt, Iterations)
	aead, err := newAEAD(passphrase, salt, Iterations)
	if err != nil {
		return err
	}
	sealed := aead.Seal(nil, nonce, plaintext, header)
	for _, chunk := range [][]byte{header, nonce, sealed} {
		if _, err := out.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

func encodeHeader(salt []byte, iterations uint32) []byte {
	header := make([]byte, 0, len(magic)+1+saltSize+4)
	header = append(header, magic...)
	header = append(header, formatVersion)
	header = append(header, salt...)
	return binary.BigEndian.AppendUint32(header, iterations)
}

func newAEAD(passphrase string, salt []byte, iterations uint32) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, int(iterations), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// open decrypts a backup and returns its parts by name.
func open(passphrase string, in io.Reader) (Manifest, map[string][]byte, error) {
	data, err := io.ReadAll(io.LimitReader(in, maxArchive+1024))
	if err != nil {
		return Manifest{}, nil, err
	}
	headerSize := len(magic) + 1 + saltSize + 4
	if len(data) < headerSize+nonceSize || string(data[:len(magic)]) != magic || data[len(magic)] != formatVersion {
		return Manifest{}, nil, ErrNotABackup
	}
	header := data[:headerSize]
	salt := header[len(magic)+1 : len(magic)+1+saltSize]
	iterations := binary.BigEndian.Uint32(header[headerSize-4:])
	if iterations < minimumIterations || iterations > 100*Iterations {
		return Manifest{}, nil, ErrNotABackup
	}
	aead, err := newAEAD(passphrase, salt, iterations)
	if err != nil {
		return Manifest{}, nil, err
	}
	nonce := data[headerSize : headerSize+nonceSize]
	plaintext, err := aead.Open(nil, nonce, data[headerSize+nonceSize:], header)
	if err != nil {
		return Manifest{}, nil, ErrWrongPassphrase
	}

	parts := map[string][]byte{}
	reader := tar.NewReader(bytes.NewReader(plaintext))
	for {
		entry, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("%w: %v", ErrNotABackup, err)
		}
		if _, known := map[string]bool{manifestName: true, databaseName: true, keyName: true, certName: true}[entry.Name]; !known {
			return Manifest{}, nil, fmt.Errorf("%w: unexpected entry %q", ErrNotABackup, entry.Name)
		}
		if parts[entry.Name], err = io.ReadAll(reader); err != nil {
			return Manifest{}, nil, err
		}
	}
	var manifest Manifest
	if err := json.Unmarshal(parts[manifestName], &manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("%w: manifest", ErrNotABackup)
	}
	for _, name := range []string{databaseName, keyName, certName} {
		sum := sha256.Sum256(parts[name])
		if parts[name] == nil || manifest.Digests[name] != fmt.Sprintf("%x", sum) {
			return Manifest{}, nil, fmt.Errorf("%w: %s", ErrInconsistent, name)
		}
	}
	return manifest, parts, nil
}

// Inspect decrypts a backup and returns its manifest without restoring it.
func Inspect(passphrase string, in io.Reader) (Manifest, error) {
	manifest, _, err := open(passphrase, in)
	return manifest, err
}

// Restore decrypts a backup and writes it to paths, which must not exist. It
// checks that the key, certificate, and manifest agree, opens the restored
// database so that its migrations run, and places a hold on sequencing that
// lasts until the node confirms it has caught up with its peers.
func Restore(ctx context.Context, passphrase string, in io.Reader, paths Paths, hostname string) (Manifest, error) {
	for _, path := range []string{paths.Database, paths.Key, paths.Cert} {
		if _, err := os.Stat(path); err == nil {
			return Manifest{}, fmt.Errorf("%w: %s exists", ErrDestinationUsed, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Manifest{}, err
		}
	}
	manifest, parts, err := open(passphrase, in)
	if err != nil {
		return Manifest{}, err
	}

	written := []string{}
	cleanup := func() {
		for _, path := range written {
			os.Remove(path)
		}
	}
	for name, path := range map[string]string{keyName: paths.Key, certName: paths.Cert, databaseName: paths.Database} {
		if err := writePrivate(path, parts[name]); err != nil {
			cleanup()
			return Manifest{}, fmt.Errorf("write %s: %w", name, err)
		}
		written = append(written, path)
	}

	identity, err := node.LoadOrCreate(paths.Key, paths.Cert, hostname)
	if err != nil || identity.Fingerprint() != manifest.Fingerprint {
		cleanup()
		return Manifest{}, errors.Join(ErrInconsistent, err)
	}
	database, err := store.Open(paths.Database)
	if err != nil {
		cleanup()
		return Manifest{}, fmt.Errorf("open restored database: %w", err)
	}
	defer database.Close()
	if err := store.NewGroupLogRepository(database).HoldSequencing(ctx); err != nil {
		cleanup()
		return Manifest{}, err
	}
	return manifest, nil
}

// writePrivate writes data to path owner-only, creating the directory
// owner-only, and refuses to replace an existing file.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
