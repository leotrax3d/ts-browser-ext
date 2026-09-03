package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
)

// logDirFor returns the directory holding the backend's log files, creating it
// if needed.
//
// The browser owns our stdout — it's the native messaging channel — and
// discards our stderr, so a log file is the only way anyone sees what the
// backend did. It sits next to the installed binary's tree on Windows and in
// the user's cache directory elsewhere.
func logDirFor() (string, error) {
	var base string
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, "AppData", "Local")
		}
		base = filepath.Join(base, "Tailscale", "BrowserExt")
	} else {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(cache, "tailscale-browser-ext")
	}
	dir := filepath.Join(base, "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// rotatingFile is an [io.Writer] that keeps a log file below a size limit,
// rolling it over to numbered siblings and discarding the oldest.
//
// A browser may keep the backend running for days, so an unbounded log file is
// not an option; and since the logs exist to be attached to a bug report, a
// handful of bounded files beats one huge one.
type rotatingFile struct {
	path    string
	maxSize int64
	keep    int // how many rolled-over files to retain, at least 1

	mu   sync.Mutex
	f    *os.File
	size int64
}

// newRotatingFile opens path for appending, rolling it over once it exceeds
// maxSize bytes and retaining keep older files alongside it.
func newRotatingFile(path string, maxSize int64, keep int) (*rotatingFile, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("maxSize must be positive, got %v", maxSize)
	}
	if keep < 1 {
		keep = 1
	}
	r := &rotatingFile{path: path, maxSize: maxSize, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f = f
	r.size = fi.Size()
	return nil
}

// rolledName returns the name of the i'th rolled-over file, counting from 1
// for the most recent.
func (r *rotatingFile) rolledName(i int) string {
	return r.path + "." + strconv.Itoa(i)
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		return 0, os.ErrClosed
	}
	// Roll over before writing rather than after, so a single write never
	// straddles two files and the limit is never exceeded by a whole message.
	if r.size > 0 && r.size+int64(len(p)) > r.maxSize {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotateLocked closes the current file, shifts the rolled-over files along by
// one, and opens a fresh file. r.mu must be held.
func (r *rotatingFile) rotateLocked() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	r.f = nil

	// Drop the oldest, then shift the rest down: .2 -> .3, .1 -> .2, and the
	// live file into .1. Renaming onto an existing name is why the oldest goes
	// first; Windows won't rename over a file that's still there.
	if err := os.Remove(r.rolledName(r.keep)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := r.keep - 1; i >= 1; i-- {
		err := os.Rename(r.rolledName(i), r.rolledName(i+1))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(r.path, r.rolledName(1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.open()
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
