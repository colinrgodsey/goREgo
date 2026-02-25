package janitor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/colinrgodsey/goREgo/pkg/config"
)

type fileEntry interface {
	Name() string
	Size() int64
	IsDir() bool
	AccessTime() time.Time
}

type fileSystem interface {
	Walk(root string, fn func(path string, info fileEntry, err error) error) error
	Remove(path string) error
}

type stdFileSystem struct{}

type stdFileEntry struct {
	info os.FileInfo
}

func (e *stdFileEntry) Name() string { return e.info.Name() }
func (e *stdFileEntry) Size() int64  { return e.info.Size() }
func (e *stdFileEntry) IsDir() bool  { return e.info.IsDir() }
func (e *stdFileEntry) AccessTime() time.Time {
	atime := e.info.ModTime()
	if stat, ok := e.info.Sys().(*syscall.Stat_t); ok {
		atime = time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
	}
	return atime
}

func (fs *stdFileSystem) Walk(root string, fn func(path string, info fileEntry, err error) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		var entry fileEntry
		if info != nil {
			entry = &stdFileEntry{info: info}
		}
		return fn(path, entry, err)
	})
}

func (fs *stdFileSystem) Remove(path string) error {
	// Best-effort attempt to make the file writable before removing.
	// This is because the storage layer now marks files as read-only.
	_ = os.Chmod(path, 0644)
	return os.Remove(path)
}

// Janitor enforces disk usage limits on the local cache by performing
// LRU eviction based on file access times. It runs cleanup only when
// triggered by cache additions, with debouncing to avoid excessive I/O.
type Janitor struct {
	rootDir     string
	maxSize     int64
	minAge      time.Duration
	debounceDur time.Duration
	mu          sync.Mutex
	fs          fileSystem

	// notifyCh receives signals when files are added to the cache
	notifyCh chan struct{}
}

func NewJanitor(cfg *config.Config) *Janitor {
	return &Janitor{
		rootDir:     cfg.LocalCache.Dir,
		maxSize:     int64(cfg.LocalCache.MaxSizeGB) * 1024 * 1024 * 1024,
		minAge:      30 * time.Second,
		debounceDur: 5 * time.Second,
		fs:          &stdFileSystem{},
		notifyCh:    make(chan struct{}, 1), // Buffered to allow non-blocking notify
	}
}

// Notify signals that a new file has been added to the local cache,
// triggering a debounced cleanup run. It is non-blocking and safe to
// call from any goroutine.
func (j *Janitor) Notify() {
	select {
	case j.notifyCh <- struct{}{}:
	default:
		// Notification already pending, skip
	}
}

// OnPut returns a callback function suitable for use with LocalStore's
// OnPutCallback. The callback triggers a debounced cleanup when files
// are added to the cache.
func (j *Janitor) OnPut() func() {
	return j.Notify
}

func (j *Janitor) Run(ctx context.Context) error {
	// Debounce timer - reset on each notification
	var debounceTimer *time.Timer
	debounceCh := make(chan time.Time, 1)

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return ctx.Err()
		case <-j.notifyCh:
			// Reset debounce timer on notification
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(j.debounceDur, func() {
				select {
				case debounceCh <- time.Now():
				default:
				}
			})
		case <-debounceCh:
			// Debounce period elapsed, run cleanup
			if err := j.Cleanup(); err != nil {
				// Log error but continue
			}
		}
	}
}

type fileInfo struct {
	path  string
	entry fileEntry
}

func (j *Janitor) Cleanup() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	var files []fileInfo
	var totalSize int64

	err := j.fs.Walk(j.rootDir, func(path string, info fileEntry, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, fileInfo{path: path, entry: info})
			totalSize += info.Size()
		}
		return nil
	})
	if err != nil {
		return err
	}

	if totalSize <= j.maxSize {
		return nil
	}

	// Sort by atime (oldest first)
	sort.Slice(files, func(i, k int) bool {
		return files[i].entry.AccessTime().Before(files[k].entry.AccessTime())
	})

	now := time.Now()
	for _, f := range files {
		if totalSize <= j.maxSize {
			break
		}

		// Safeguard: Don't delete files accessed within the last minAge (default 30s)
		// This prevents race conditions where a file is deleted right after creation/access
		// during execution.
		if now.Sub(f.entry.AccessTime()) < j.minAge {
			continue
		}

		if err := j.fs.Remove(f.path); err != nil {
			continue
		}
		totalSize -= f.entry.Size()
	}

	return nil
}
