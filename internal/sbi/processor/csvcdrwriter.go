package processor

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type RotatingCSVWriter struct {
	dir          string
	prefix       string
	maxSizeBytes int64
	maxAge       time.Duration
	header       []string

	mu       sync.Mutex
	f        *os.File
	w        *csv.Writer
	openedAt time.Time
	currPath string
	lockFile *os.File
}

func NewRotatingCSVWriter(dir, prefix string, maxSizeBytes int64, maxAge time.Duration, header []string) (*RotatingCSVWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, ".cdr.lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	return &RotatingCSVWriter{
		dir:          dir,
		prefix:       prefix,
		maxSizeBytes: maxSizeBytes,
		maxAge:       maxAge,
		header:       header,
		lockFile:     lf,
	}, nil
}

func (r *RotatingCSVWriter) lock() error { return syscall.Flock(int(r.lockFile.Fd()), syscall.LOCK_EX) }
func (r *RotatingCSVWriter) unlock()     { _ = syscall.Flock(int(r.lockFile.Fd()), syscall.LOCK_UN) }

// openNewLocked creates a new ".open" file and writes the header if empty.
func (r *RotatingCSVWriter) openNewLocked() error {
	ts := time.Now().UTC().Format("20060102-150405")
	openName := fmt.Sprintf("%s-%s.open", r.prefix, ts)
	path := filepath.Join(r.dir, openName)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)

	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		if err := w.Write(r.header); err != nil {
			_ = f.Close()
			return err
		}
		w.Flush()
		if err := w.Error(); err != nil {
			_ = f.Close()
			return err
		}
	}

	r.f = f
	r.w = w
	r.openedAt = time.Now().UTC()
	r.currPath = path
	return nil
}

// ensureOpenLocked rotates if needed, then guarantees a writer is open.
func (r *RotatingCSVWriter) ensureOpenLocked() error {
	if r.f != nil {
		needRotate := false
		if time.Since(r.openedAt) >= r.maxAge {
			needRotate = true
		} else if fi, err := r.f.Stat(); err == nil && fi.Size() >= r.maxSizeBytes {
			needRotate = true
		}
		if needRotate {
			if err := r.rotateLocked(); err != nil {
				return err
			}
		}
	}
	if r.f == nil {
		return r.openNewLocked()
	}
	return nil
}

func (r *RotatingCSVWriter) rotateLocked() error {
	if r.f == nil {
		return errors.New("rotate with no open file")
	}
	// Close current writer/file
	if err := r.closeCurrentLocked(); err != nil {
		return err
	}
	// Rename ".open" to final ".csv"
	finalPath := strings.TrimSuffix(r.currPath, ".open") + ".csv"
	if err := os.Rename(r.currPath, finalPath); err != nil {
		return fmt.Errorf("rename to final: %w", err)
	}
	// Leave r.f/r.w nil; next ensureOpenLocked() will open a new file
	r.f, r.w, r.currPath = nil, nil, ""
	return nil
}

func (r *RotatingCSVWriter) closeCurrentLocked() error {
	if r.w != nil {
		r.w.Flush()
		if err := r.w.Error(); err != nil {
			_ = r.f.Close()
			r.w = nil
			r.f = nil
			return err
		}
	}
	if r.f != nil {
		if err := r.f.Close(); err != nil {
			r.w = nil
			r.f = nil
			return err
		}
	}
	r.w = nil
	r.f = nil
	return nil
}

func (r *RotatingCSVWriter) WriteRow(row []string) error {
	if err := r.lock(); err != nil {
		return err
	}
	defer r.unlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.ensureOpenLocked(); err != nil {
		return err
	}
	if r.w == nil { // extra safety
		if err := r.openNewLocked(); err != nil {
			return err
		}
	}

	if err := r.w.Write(row); err != nil {
		return err
	}
	r.w.Flush()
	if err := r.w.Error(); err != nil {
		return err
	}

	// Rotate immediately if size crossed threshold after write
	if fi, err := r.f.Stat(); err == nil && fi.Size() >= r.maxSizeBytes {
		if err := r.rotateLocked(); err != nil {
			return err
		}
	}
	return nil
}

// Close finalizes the current ".open" to ".csv" (if any).
func (r *RotatingCSVWriter) Close() error {
	if err := r.lock(); err != nil {
		return err
	}
	defer r.unlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		return nil
	}
	return r.rotateLocked()
}
