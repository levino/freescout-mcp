package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Temporary uploads: a file sent to POST /mcp/api/v1/uploads waits here until
// a draft, reply or forward names it by upload_id, then it is stored in
// FreeScout and deleted here. Nothing reaches FreeScout before that.
//
// The files live in a directory of their own that is emptied on start: the
// index is in memory, so whatever a previous process left behind is orphaned.
const (
	uploadTTL          = time.Hour
	maxUploadsPerUser  = 20
	maxUploadBytesUser = 100 << 20
)

var uploadIDRe = regexp.MustCompile(`^up_[0-9a-f]{32}$`)

type upload struct {
	ID       string    `json:"upload_id"`
	Owner    string    `json:"-"`
	Name     string    `json:"filename"`
	MimeType string    `json:"mime_type,omitempty"`
	Size     int       `json:"size"`
	Expires  time.Time `json:"expires_at"`
	path     string
}

type uploadStore struct {
	mu    sync.Mutex
	dir   string
	items map[string]*upload
	now   func() time.Time
}

func newUploadStore(dir string) (*uploadStore, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "freescout-mcp-uploads")
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &uploadStore{dir: dir, items: map[string]*upload{}, now: time.Now}, nil
}

var errUploadLimit = errors.New("too many pending uploads")

// put stores one file for owner. The name only travels as metadata; the file
// on disk is named after the random id, so no client input reaches a path.
func (s *uploadStore) put(owner, name, mimeType string, data []byte) (*upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	count, bytes := 0, 0
	for _, u := range s.items {
		if u.Owner == owner {
			count++
			bytes += u.Size
		}
	}
	if count >= maxUploadsPerUser || bytes+len(data) > maxUploadBytesUser {
		return nil, fmt.Errorf("%w: at most %d files and %d MB per user, unused ones expire after an hour",
			errUploadLimit, maxUploadsPerUser, maxUploadBytesUser>>20)
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	id := "up_" + hex.EncodeToString(raw)
	path := filepath.Join(s.dir, id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}

	u := &upload{ID: id, Owner: owner, Name: name, MimeType: mimeType, Size: len(data), Expires: s.now().Add(uploadTTL), path: path}
	s.items[id] = u
	return u, nil
}

// read returns the file if it exists, has not expired and belongs to owner.
// Someone else's id looks exactly like an unknown one.
func (s *uploadStore) read(owner, id string) (*upload, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	u := s.items[id]
	if !uploadIDRe.MatchString(id) || u == nil || u.Owner != owner {
		return nil, nil, fmt.Errorf("upload %s not found (uploads expire after an hour and can be used once)", id)
	}
	data, err := os.ReadFile(u.path)
	if err != nil {
		return nil, nil, fmt.Errorf("upload %s is gone", id)
	}
	return u, data, nil
}

func (s *uploadStore) remove(owner, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.items[id]
	if u == nil || u.Owner != owner {
		return false
	}
	os.Remove(u.path)
	delete(s.items, id)
	return true
}

func (s *uploadStore) sweepLocked() {
	now := s.now()
	for id, u := range s.items {
		if now.After(u.Expires) {
			os.Remove(u.path)
			delete(s.items, id)
		}
	}
}

func (s *uploadStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
}
