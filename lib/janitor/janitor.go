package janitor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/colinrgodsey/goREgo/lib/config"
)

type Janitor struct {
	rootDir   string
	maxSize   int64
	checkFreq time.Duration
	mu        sync.Mutex
}

func NewJanitor(cfg *config.Config) *Janitor {
	return &Janitor{
		rootDir:   cfg.LocalCacheDir,
		maxSize:   int64(cfg.LocalCacheMaxSizeGB) * 1024 * 1024 * 1024,
		checkFreq: 1 * time.Minute,
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
	path string
	info os.FileInfo
	atime time.Time
}

func (j *Janitor) Cleanup() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	var files []fileInfo
	var totalSize int64

	err := filepath.Walk(j.rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			atime := info.ModTime()
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				atime = time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
			}
			files = append(files, fileInfo{path: path, info: info, atime: atime})
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
		return files[i].atime.Before(files[k].atime)
	})

	for _, f := range files {
		if totalSize <= j.maxSize {
			break
		}
		if err := os.Remove(f.path); err != nil {
			continue
		}
		totalSize -= f.info.Size()
	}

	return nil
}
