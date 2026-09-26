package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// hashEntry maps one content hash to the session/project minted for it.
// Content is never stored — only the 64-hex digest plus the two IDs.
type hashEntry struct {
	Session  string `json:"session"`
	Project  string `json:"project"`
	Created  int64  `json:"created"`
	LastSeen int64  `json:"last_seen"`
}

// hashStore is a TTL map of content-hash to identity, persisted to disk.
// Entries unseen for longer than ttl are purged ("3d一清").
type hashStore struct {
	mu       sync.Mutex
	m        map[string]*hashEntry
	path     string
	ttl      time.Duration
	maxN     int
	dirty    bool
	lastSave time.Time
	stop     chan struct{}
	wg       sync.WaitGroup
}

func openStore(path string, ttl time.Duration, maxN int) (*hashStore, error) {
	s := &hashStore{
		m:    make(map[string]*hashEntry),
		path: path,
		ttl:  ttl,
		maxN: maxN,
		stop: make(chan struct{}),
	}
	if data, err := os.ReadFile(path); err == nil {
		var disk map[string]*hashEntry
		if err := json.Unmarshal(data, &disk); err == nil {
			now := time.Now().Unix()
			for h, e := range disk {
				if e == nil || e.Session == "" || len(h) != 64 {
					continue
				}
				if now-e.LastSeen > int64(ttl.Seconds()) {
					continue
				}
				s.m[h] = e
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	s.wg.Add(1)
	go s.sweeper()
	return s, nil
}

// get returns a copy of the entry and refreshes its sliding window.
func (s *hashStore) get(hash string) (*hashEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[hash]
	if !ok {
		return nil, false
	}
	now := time.Now().Unix()
	if now-e.LastSeen > int64(s.ttl.Seconds()) {
		delete(s.m, hash)
		s.dirty = true
		return nil, false
	}
	e.LastSeen = now
	s.dirty = true
	s.maybeSaveLocked()
	cp := *e
	return &cp, true
}

// put records or refreshes the identity for a hash.
func (s *hashStore) put(hash, session, project string) {
	if len(hash) != 64 || session == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	if e, ok := s.m[hash]; ok {
		e.Session = session
		e.Project = project
		e.LastSeen = now
	} else {
		if len(s.m) >= s.maxN {
			s.evictLocked(now)
		}
		s.m[hash] = &hashEntry{Session: session, Project: project, Created: now, LastSeen: now}
	}
	s.dirty = true
	s.maybeSaveLocked()
}

func (s *hashStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// evictLocked drops expired entries first, then the stalest ones.
func (s *hashStore) evictLocked(now int64) {
	ttl := int64(s.ttl.Seconds())
	for h, e := range s.m {
		if now-e.LastSeen > ttl {
			delete(s.m, h)
		}
	}
	for len(s.m) >= s.maxN {
		var oldest string
		var oldestSeen int64
		first := true
		for h, e := range s.m {
			if first || e.LastSeen < oldestSeen {
				oldest, oldestSeen, first = h, e.LastSeen, false
			}
		}
		if oldest == "" {
			break
		}
		delete(s.m, oldest)
	}
}

// maybeSaveLocked persists at most once every 10 seconds.
func (s *hashStore) maybeSaveLocked() {
	if !s.dirty || time.Since(s.lastSave) < 10*time.Second {
		return
	}
	s.saveLocked()
}

// saveLocked writes atomically: temp file + rename, mode 0600.
func (s *hashStore) saveLocked() {
	data, err := json.Marshal(s.m)
	if err != nil {
		return
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".hashes-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return
	}
	s.dirty = false
	s.lastSave = time.Now()
}

func (s *hashStore) sweeper() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now().Unix()
			ttl := int64(s.ttl.Seconds())
			for h, e := range s.m {
				if now-e.LastSeen > ttl {
					delete(s.m, h)
					s.dirty = true
				}
			}
			if s.dirty {
				s.saveLocked()
			}
			s.mu.Unlock()
		}
	}
}

func (s *hashStore) close() {
	close(s.stop)
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.saveLocked()
	}
}
