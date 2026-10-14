package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxStateBytes = 16 << 20

type Store struct{ Root string }
type Session struct {
	State State
	store *Store
	lock  *os.File
}

// NewStore scopes local metadata to the exact Podman endpoint, not a mutable connection nickname.
func NewStore(root, endpoint string) (*Store, error) {
	if endpoint == "" {
		return nil, errors.New("state requires an explicit Podman endpoint")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = privateDir(absolute); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(endpoint))
	dir := filepath.Join(absolute, hex.EncodeToString(hash[:16]))
	if err = privateDir(dir); err != nil {
		return nil, err
	}
	return &Store{Root: dir}, nil
}

func privateDir(p string) error {
	if err := os.MkdirAll(p, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("state directory must be a private non-symlink directory: %s", p)
	}
	return nil
}

func (s *Store) Snapshot() (State, error) {
	p := filepath.Join(s.Root, "state.json")
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: 1}, nil
	}
	if err != nil {
		return State{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxStateBytes {
		return State{}, errors.New("state must be a private regular file within size limit")
	}
	f, err := os.Open(p)
	if err != nil {
		return State{}, err
	}
	defer f.Close()
	var state State
	decoder := json.NewDecoder(io.LimitReader(f, maxStateBytes+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return State{}, errors.New("unexpected trailing state content")
	}
	if state.Version != 1 {
		return State{}, errors.New("unsupported state schema; refusing mutation")
	}
	return state, nil
}

// Begin fails fast on cross-process contention. Readers use the last atomic snapshot without waiting.
func (s *Store) Begin() (*Session, error) {
	p := filepath.Join(s.Root, "operation.lock")
	if info, err := os.Lstat(p); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("unsafe state lock")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	state, err := s.Snapshot()
	if err != nil {
		unlockFile(f)
		f.Close()
		return nil, err
	}
	return &Session{State: state, store: s, lock: f}, nil
}

func (s *Session) Close() error {
	if s.lock == nil {
		return nil
	}
	err := unlockFile(s.lock)
	closeErr := s.lock.Close()
	s.lock = nil
	return errors.Join(err, closeErr)
}

func (s *Session) Save() error {
	if s.lock == nil {
		return errors.New("state session is closed")
	}
	data, err := json.MarshalIndent(s.State, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		return errors.New("state exceeds size limit")
	}
	target := filepath.Join(s.store.Root, "state.json")
	if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		return errors.New("refusing non-regular state target")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(s.store.Root, ".state-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, target); err != nil {
		return err
	}
	return syncDirectory(s.store.Root)
}

func (s *Store) SourceDir(id string) (string, error) {
	if b, err := hex.DecodeString(id); err != nil || len(b) != 16 {
		return "", errors.New("invalid source identity")
	}
	p := filepath.Join(s.Root, "sources", id)
	if err := privateDir(p); err != nil {
		return "", err
	}
	return p, nil
}
