package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RotatingFile is an io.WriteCloser writing to `<dir>/<prefix>-YYYY-MM-DD.log`, rotating
// daily and when MaxSize is exceeded (`.1.log` … `.N.log`). Writes are serialized, so it
// is safe to share between goroutines and handlers.
type RotatingFile struct {
	Dir      string
	Prefix   string // default "app"
	MaxSize  int64  // bytes, default 10 MiB
	MaxFiles int    // rotated files kept per day, default 5
	Now      func() time.Time

	mu   sync.Mutex
	f    *os.File
	path string
	size int64
}

func (r *RotatingFile) defaults() {
	if r.Prefix == "" {
		r.Prefix = "app"
	}
	if r.MaxSize <= 0 {
		r.MaxSize = 10 << 20
	}
	if r.MaxFiles <= 0 {
		r.MaxFiles = 5
	}
	if r.Now == nil {
		r.Now = func() time.Time { return time.Now().UTC() }
	}
}

// Write implements io.Writer.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults()
	target := filepath.Join(r.Dir, fmt.Sprintf("%s-%s.log", r.Prefix, r.Now().Format(time.DateOnly)))
	if target != r.path || r.f == nil {
		if err := r.open(target); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.MaxSize {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close closes the current file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

func (r *RotatingFile) open(target string) error {
	if r.f != nil {
		_ = r.f.Close()
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.path, r.size = f, target, st.Size()
	return nil
}

func (r *RotatingFile) rotate() error {
	_ = r.f.Close()
	r.f = nil
	base := r.path[:len(r.path)-len(".log")]
	_ = os.Remove(fmt.Sprintf("%s.%d.log", base, r.MaxFiles))
	for i := r.MaxFiles - 1; i >= 1; i-- {
		if err := os.Rename(fmt.Sprintf("%s.%d.log", base, i), fmt.Sprintf("%s.%d.log", base, i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(r.path, base+".1.log"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.open(r.path)
}
