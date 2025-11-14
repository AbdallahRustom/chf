package processor

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const completionLine = "GMT File administratively closed"

type RotatingCSVWriter struct {
	// Base directory contains current/, archive/, export/
	baseDir    string
	currentDir string
	archiveDir string
	exportDir  string

	prefix         string
	maxSizeBytes   int64
	rotateInterval time.Duration
	header         []string

	// State
	mu          sync.Mutex
	f           *os.File
	w           *csv.Writer
	openedAt    time.Time
	currBase    string // <prefix>-<UTC>-<INDEX>
	wroteRows   bool   // header written only when the first data row arrives
	index       uint64
	statePath   string
	lockFile    *os.File
	lastEmitted time.Time

	// background ticker to ensure a file at least every rotateInterval
	bgOnce sync.Once
	stopCh chan struct{}
}

func NewRotatingCSVWriter(baseDir, prefix string, maxSizeBytes int64, rotateEvery time.Duration, header []string) (*RotatingCSVWriter, error) {
	currentDir := filepath.Join(baseDir, "current")
	archiveDir := filepath.Join(baseDir, "archive")
	exportDir := filepath.Join(baseDir, "export")
	for _, d := range []string{currentDir, archiveDir, exportDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	lockPath := filepath.Join(baseDir, ".cdr.lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	r := &RotatingCSVWriter{
		baseDir:        baseDir,
		currentDir:     currentDir,
		archiveDir:     archiveDir,
		exportDir:      exportDir,
		prefix:         prefix,
		maxSizeBytes:   maxSizeBytes,
		rotateInterval: rotateEvery,
		header:         header,
		statePath:      filepath.Join(baseDir, "state.json"),
		lockFile:       lf,
		stopCh:         make(chan struct{}),
	}
	idx, err := r.loadOrDiscoverIndex()
	if err != nil {
		return nil, fmt.Errorf("load index: %w", err)
	}
	r.index = idx
	r.lastEmitted = time.Now().UTC()
	return r, nil
}

func (r *RotatingCSVWriter) lock() error { return syscall.Flock(int(r.lockFile.Fd()), syscall.LOCK_EX) }
func (r *RotatingCSVWriter) unlock()     { _ = syscall.Flock(int(r.lockFile.Fd()), syscall.LOCK_UN) }

// StartBackground runs a light ticker so we emit at least one file every rotateInterval (even empty).
func (r *RotatingCSVWriter) StartBackground() {
	r.bgOnce.Do(func() {
		go func() {
			tick := time.NewTicker(1 * time.Minute)
			defer tick.Stop()
			for {
				select {
				case <-r.stopCh:
					return
				case <-tick.C:
					_ = r.Tick()
				}
			}
		}()
	})
}

// ensureOpenLocked rotates if needed and ensures a writer is open.
func (r *RotatingCSVWriter) ensureOpenLocked() error {
	if r.f != nil {
		needRotate := false
		if time.Since(r.openedAt) >= r.rotateInterval {
			needRotate = true
		} else if fi, err := r.f.Stat(); err == nil && fi.Size() >= r.maxSizeBytes {
			needRotate = true
		}
		if needRotate {
			if err := r.finalizeAndPublishLocked(); err != nil {
				return err
			}
		}
	}
	if r.f == nil {
		return r.openNewLocked()
	}
	return nil
}

func (r *RotatingCSVWriter) openNewLocked() error {
	ts := time.Now().UTC().Format("20060102T150405Z")
	name := fmt.Sprintf("%s-%s-%08X", r.prefix, ts, r.index)
	partPath := filepath.Join(r.currentDir, name+".csv.part")

	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}

	r.f = f
	r.w = csv.NewWriter(f)
	r.openedAt = time.Now().UTC()
	r.currBase = name
	r.wroteRows = false // header is deferred until first data row
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
	if r.w == nil {
		if err := r.openNewLocked(); err != nil {
			return err
		}
	}
	// Write header once, only when a first data row comes in
	if !r.wroteRows && len(r.header) > 0 {
		if err := r.w.Write(r.header); err != nil {
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
	r.wroteRows = true

	// Rotate if size threshold crossed
	if fi, err := r.f.Stat(); err == nil && fi.Size() >= r.maxSizeBytes {
		if err := r.finalizeAndPublishLocked(); err != nil {
			return err
		}
	}
	return nil
}

// Tick ensures we emit a file at least every rotateInterval.
// If no rows were written, it emits an "empty" file with only the completion line.
func (r *RotatingCSVWriter) Tick() error {
	if err := r.lock(); err != nil {
		return err
	}
	defer r.unlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.lastEmitted) < r.rotateInterval {
		return nil
	}
	if r.f == nil {
		if err := r.openNewLocked(); err != nil {
			return err
		}
	}
	return r.finalizeAndPublishLocked()
}

// finalizeAndPublishLocked:
// - flushes CSV
// - appends completion line
// - fsync + close
// - renames .csv.part -> .csv (atomic)
// - copies to archive via .part then rename (atomic publish)
// - moves to export (atomic)
// - increments and persists the hex index
func (r *RotatingCSVWriter) finalizeAndPublishLocked() error {
	if r.f == nil {
		return errors.New("finalize with no open file")
	}

	// Flush CSV
	if r.w != nil {
		r.w.Flush()
		if err := r.w.Error(); err != nil {
			_ = r.f.Close()
			r.w, r.f = nil, nil
			return err
		}
	}

	// Append completion line regardless of wroteRows (so empty files contain only this)
	if _, err := r.f.WriteString(completionTrailer()); err != nil {
		_ = r.f.Close()
		r.w, r.f = nil, nil
		return fmt.Errorf("write completion line: %w", err)
	}
	if err := r.f.Sync(); err != nil {
		_ = r.f.Close()
		r.w, r.f = nil, nil
		return fmt.Errorf("fsync: %w", err)
	}
	if err := r.f.Close(); err != nil {
		r.w, r.f = nil, nil
		return err
	}
	r.w, r.f = nil, nil

	partPath := filepath.Join(r.currentDir, r.currBase+".csv.part")
	finalCurrent := filepath.Join(r.currentDir, r.currBase+".csv")

	// Atomic finalize in current: .part -> .csv
	if err := os.Rename(partPath, finalCurrent); err != nil {
		return fmt.Errorf("rename to final current: %w", err)
	}

	// Copy to archive with .part then atomic rename
	archFinal := filepath.Join(r.archiveDir, filepath.Base(finalCurrent))
	archTmp := archFinal + ".part"
	if err := copyFile(finalCurrent, archTmp); err != nil {
		return fmt.Errorf("copy to archive tmp: %w", err)
	}
	if err := os.Rename(archTmp, archFinal); err != nil {
		return fmt.Errorf("archive publish rename: %w", err)
	}

	// Atomic move to export
	exportFinal := filepath.Join(r.exportDir, filepath.Base(finalCurrent))
	if err := os.Rename(finalCurrent, exportFinal); err != nil {
		return fmt.Errorf("export move: %w", err)
	}

	// Advance hex index and persist
	r.index++
	if err := r.storeIndex(r.index); err != nil {
		return fmt.Errorf("store index: %w", err)
	}
	r.currBase = ""
	r.lastEmitted = time.Now().UTC()
	return nil
}

func (r *RotatingCSVWriter) Close() error {
	close(r.stopCh)

	if err := r.lock(); err != nil {
		return err
	}
	defer r.unlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		return nil
	}
	return r.finalizeAndPublishLocked()
}

// ---- Index state ----

func (r *RotatingCSVWriter) loadOrDiscoverIndex() (uint64, error) {
	// Try state.json
	type st struct {
		LastIndex uint64 `json:"last_index"`
	}
	if b, err := os.ReadFile(r.statePath); err == nil && len(b) > 0 {
		var s st
		if err := json.Unmarshal(b, &s); err == nil {
			return s.LastIndex + 1, nil
		}
	}

	// Discover from filenames in archive/export/current
	maxIdx := int64(-1)
	checkDir := func(d string) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			// Accept .csv and .csv.part
			if !(strings.HasSuffix(name, ".csv") || strings.HasSuffix(name, ".csv.part")) {
				continue
			}
			if idx, ok := parseIndexFromName(name); ok {
				if int64(idx) > maxIdx {
					maxIdx = int64(idx)
				}
			}
		}
	}
	for _, d := range []string{r.archiveDir, r.exportDir, r.currentDir} {
		checkDir(d)
	}
	if maxIdx >= 0 {
		return uint64(maxIdx + 1), nil
	}
	return 0, nil
}

func (r *RotatingCSVWriter) storeIndex(idx uint64) error {
	tmp := r.statePath + ".part"
	type st struct {
		LastIndex uint64 `json:"last_index"`
	}
	b, _ := json.Marshal(st{LastIndex: idx})
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.statePath)
}

// <prefix>-YYYYmmddTHHMMSSZ-XXXXXXXX.csv[.part]
func parseIndexFromName(name string) (uint64, bool) {
	base := strings.TrimSuffix(name, ".part")
	base = strings.TrimSuffix(base, ".csv")
	parts := strings.Split(base, "-")
	if len(parts) < 3 {
		return 0, false
	}
	hexPart := parts[len(parts)-1]
	// hex preferred, decimal allowed
	if v, err := strconv.ParseUint(hexPart, 16, 64); err == nil {
		return v, true
	}
	if v, err := strconv.ParseUint(hexPart, 10, 64); err == nil {
		return v, true
	}
	return 0, false
}

// Safe copy with fsync on destination
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func completionTrailer() string {
	return fmt.Sprintf("%s %s\n", time.Now().UTC().Format(time.RFC3339), completionLine)
}
