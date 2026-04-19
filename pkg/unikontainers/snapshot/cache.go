// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package snapshot

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const DefaultCacheRoot = "/var/lib/urunc/snapshots"

// Cache is a content-addressed store of firecracker template snapshots.
//
// Each template lives in <root>/<cache-key>/ and contains:
//   - vmstate         (firecracker snapshot state)
//   - mem             (firecracker guest memory image)
//   - manifest.json   (sidecar metadata)
//   - .lock           (exclusive flock target used during first-run builds)
type Cache struct {
	Root string
}

func NewCache(root string) *Cache {
	if root == "" {
		root = DefaultCacheRoot
	}
	return &Cache{Root: root}
}

// Entry is a resolved cache location for a key. The directory is created if
// it doesn't exist. Presence of a readable manifest indicates the template
// is ready for restore.
type Entry struct {
	Key      string
	Dir      string
	Vmstate  string
	Mem      string
	Manifest string
}

func (c *Cache) Entry(key string) (*Entry, error) {
	if err := os.MkdirAll(c.Root, 0o755); err != nil {
		return nil, fmt.Errorf("create cache root: %w", err)
	}
	dir := filepath.Join(c.Root, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &Entry{
		Key:      key,
		Dir:      dir,
		Vmstate:  filepath.Join(dir, VmstateFilename),
		Mem:      filepath.Join(dir, MemFilename),
		Manifest: filepath.Join(dir, ManifestFilename),
	}, nil
}

// HasTemplate reports whether a readable manifest.json exists in this entry.
// A build is considered complete only once the manifest has been renamed
// into place atomically.
func (e *Entry) HasTemplate() bool {
	_, err := os.Stat(e.Manifest)
	return err == nil
}

// Lock is an advisory-exclusive flock on <dir>/.lock. The Close method
// releases the lock and closes the underlying fd.
type Lock struct {
	f *os.File
}

func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}

// AcquireLock opens <dir>/.lock and takes an exclusive flock. The call blocks
// until the lock is obtained.
func (e *Entry) AcquireLock() (*Lock, error) {
	lockPath := filepath.Join(e.Dir, LockFilename)
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return &Lock{f: f}, nil
}

// TempPaths returns the .tmp filenames used during a build so the caller
// can direct firecracker /snapshot/create at them before renaming into place.
func (e *Entry) TempPaths() (vmstateTmp, memTmp string) {
	return e.Vmstate + ".tmp", e.Mem + ".tmp"
}

// CommitBuild renames the .tmp snapshot files into their final names and
// atomically writes the manifest. Must be called under AcquireLock. On any
// error, stale .tmp files are removed so a retry starts clean.
func (e *Entry) CommitBuild(m *Manifest) error {
	vmstateTmp, memTmp := e.TempPaths()
	if err := os.Rename(vmstateTmp, e.Vmstate); err != nil {
		_ = os.Remove(vmstateTmp)
		_ = os.Remove(memTmp)
		return fmt.Errorf("rename vmstate: %w", err)
	}
	if err := os.Rename(memTmp, e.Mem); err != nil {
		_ = os.Remove(memTmp)
		return fmt.Errorf("rename mem: %w", err)
	}
	if err := WriteManifest(e.Dir, m); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}
