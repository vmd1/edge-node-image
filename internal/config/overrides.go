package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Overrides is the out-of-band path (nodectl config set): a Spec on /var that beats the CRD but expires, so a forgotten
// override cannot hold a node away from Git. Default TTL 4 h, maximum 7 d.
type Overrides struct {
	Path string
	Now  func() time.Time
	mu   sync.Mutex
}

const (
	DefaultTTL = 4 * time.Hour
	MaxTTL     = 7 * 24 * time.Hour
)

type overrideFile struct {
	Spec    Spec      `json:"spec"`
	Expires time.Time `json:"expires"`
}

func (o *Overrides) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Set validates and stores an override. ttl 0 means the default.
func (o *Overrides) Set(s Spec, ttl time.Duration) (time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, err
	}
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < 0 || ttl > MaxTTL {
		return time.Time{}, errors.New("ttl must be between 1s and 168h")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	f := overrideFile{Spec: s, Expires: o.now().Add(ttl)}
	b, _ := json.Marshal(f)
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o755); err != nil {
		return time.Time{}, err
	}
	tmp := o.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return time.Time{}, err
	}
	return f.Expires, os.Rename(tmp, o.Path)
}

// Active returns the override if present and not expired; an expired file is removed.
func (o *Overrides) Active() *Spec {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	b, err := os.ReadFile(o.Path)
	if err != nil {
		return nil
	}
	var f overrideFile
	if json.Unmarshal(b, &f) != nil || !o.now().Before(f.Expires) {
		_ = os.Remove(o.Path)
		return nil
	}
	return &f.Spec
}

func (o *Overrides) Clear() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := os.Remove(o.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
