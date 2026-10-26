package tlsprofile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	p "tack.local/management/protocol"
)

// Identity contains template entropy, never TLS cryptographic randomness. It is
// read once when constructing a client and is immutable for that client's life.
type Identity struct{ seed [32]byte }

type identityRecord struct {
	Version int    `json:"version"`
	Seed    string `json:"seed"`
}

// LoadIdentity atomically creates an identity if absent. Existing invalid files
// fail closed instead of silently changing the fingerprint. Parent directories
// must already exist. Concurrent creators publish with no replacement.
func LoadIdentity(path string) (*Identity, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("TLS identity file must be an absolute path")
	}
	identity, err := readIdentity(path)
	if !os.IsNotExist(err) {
		return identity, err
	}
	identity = new(Identity)
	if _, err := rand.Read(identity.seed[:]); err != nil {
		return nil, err
	}
	data, err := json.Marshal(identityRecord{Version: 1, Seed: hex.EncodeToString(identity.seed[:])})
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tls-identity-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Link(f.Name(), path); err != nil {
		if os.IsExist(err) {
			return readIdentity(path)
		}
		return nil, err
	}
	if err = p.SyncDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return identity, nil
}

func readIdentity(path string) (*Identity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return nil, errors.New("invalid TLS identity file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var record identityRecord
	if err := p.Decode(io.LimitReader(f, 1025), &record); err != nil {
		return nil, errors.New("invalid TLS identity file; refusing to regenerate")
	}
	seed, err := hex.DecodeString(record.Seed)
	if err != nil || record.Version != 1 || len(seed) != 32 {
		return nil, errors.New("unsupported or invalid TLS identity; refusing to regenerate")
	}
	identity := new(Identity)
	copy(identity.seed[:], seed)
	return identity, nil
}
