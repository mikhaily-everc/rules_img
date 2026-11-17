package registry

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// DefaultUploadTTL is how long an upload session may sit idle before its spool file is reclaimed.
const DefaultUploadTTL = time.Hour

var (
	errUploadOffset   = errors.New("content range does not match the upload's size")
	errUploadStreamed = errors.New("stream uploads after first write are not allowed")
)

// uploadSession is one chunked upload, spooled to a file so memory stays O(chunk), not O(blob).
type uploadSession struct {
	mu      sync.Mutex
	file    *os.File
	size    int64
	touched time.Time
}

// uploadStore holds the open sessions. Its lock guards only the map; I/O runs under each session's.
type uploadStore struct {
	dir string // "" means os.TempDir()
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*uploadSession
}

func newUploadStore(dir string, ttl time.Duration) *uploadStore {
	if ttl <= 0 {
		ttl = DefaultUploadTTL
	}
	return &uploadStore{dir: dir, ttl: ttl, now: time.Now, sessions: map[string]*uploadSession{}}
}

// session returns id's session, creating it when create is set; created reports a new one.
func (s *uploadStore) session(id string, create bool) (sess *uploadSession, created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		return sess, false, nil
	}
	if !create {
		return nil, false, nil
	}
	f, err := os.CreateTemp(s.dir, "upload-*")
	if err != nil {
		return nil, false, err
	}
	sess = &uploadSession{file: f, touched: s.now()}
	s.sessions[id] = sess
	return sess, true, nil
}

// appendChunk streams r onto id's spool. start is the Content-Range offset, or -1 for a
// streamed PATCH, which may only be a session's first write. It returns the new size.
func (s *uploadStore) appendChunk(id string, start int64, r io.Reader) (int64, error) {
	sess, created, err := s.session(id, true)
	if err != nil {
		return 0, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	switch {
	case start < 0 && !created:
		return 0, errUploadStreamed
	case start >= 0 && start != sess.size:
		return 0, errUploadOffset
	}
	n, err := io.Copy(sess.file, r)
	sess.size += n
	sess.touched = s.now()
	return sess.size, err
}

// take removes id's session for its final PUT. The caller reads it via reader and must discard it.
func (s *uploadStore) take(id string) *uploadSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	return sess
}

// reader replays the spooled bytes from the start without loading them.
func (sess *uploadSession) reader() io.Reader {
	return io.NewSectionReader(sess.file, 0, sess.size)
}

// discard closes and deletes the spool file.
func (sess *uploadSession) discard() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	name := sess.file.Name()
	sess.file.Close()
	os.Remove(name)
}

// sweep discards sessions idle longer than the TTL and returns how many it removed.
func (s *uploadStore) sweep() int {
	cutoff := s.now().Add(-s.ttl)
	var stale []*uploadSession
	s.mu.Lock()
	for id, sess := range s.sessions {
		sess.mu.Lock()
		idle := sess.touched.Before(cutoff)
		sess.mu.Unlock()
		if idle {
			stale = append(stale, sess)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, sess := range stale {
		sess.discard()
	}
	return len(stale)
}
