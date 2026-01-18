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

type Janitor struct {
	rootDir   string
	maxSize   int64
	checkFreq time.Duration
	mu        sync.Mutex
	fs        fileSystem
}

func NewJanitor(cfg *config.Config) *Janitor {
	return &Janitor{
		rootDir:   cfg.LocalCacheDir,
		maxSize:   int64(cfg.LocalCacheMaxSizeGB) * 1024 * 1024 * 1024,
		checkFreq: 1 * time.Minute,
		fs:        &stdFileSystem{},
	}
}

func (j *Janitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(j.checkFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
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

	for _, f := range files {
		if totalSize <= j.maxSize {
			break
		}
		if err := j.fs.Remove(f.path); err != nil {
			continue
		}
		totalSize -= f.entry.Size()
	}

	return nil
}
