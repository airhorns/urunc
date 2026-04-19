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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestComputeKeyDeterministic(t *testing.T) {
	tmp := t.TempDir()
	bin := writeTempFile(t, tmp, "unikernel", []byte("unikernel bytes"))

	in := KeyInputs{
		UnikernelBinaryPath: bin,
		UnikernelType:       "unikraft",
		UnikernelVersion:    "v1",
		Hypervisor:          "firecracker",
		FCBinaryPath:        "/usr/local/bin/firecracker",
		FCVersion:           "abc123",
		Arch:                "amd64",
		MemMiB:              256,
		VCPUs:               1,
		GuestCmdLine:        "/unikernel -arg",
	}
	k1, sha1, err := ComputeKey(in)
	if err != nil {
		t.Fatal(err)
	}
	k2, sha2, err := ComputeKey(in)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("key not deterministic: %s vs %s", k1, k2)
	}
	if sha1 != sha2 {
		t.Fatalf("unikernel sha not deterministic: %s vs %s", sha1, sha2)
	}
	if len(k1) != 64 {
		t.Fatalf("expected hex sha256 (64 chars), got %d", len(k1))
	}
}

func TestComputeKeyChangesWithInputs(t *testing.T) {
	tmp := t.TempDir()
	bin := writeTempFile(t, tmp, "unikernel", []byte("same bytes"))

	base := KeyInputs{
		UnikernelBinaryPath: bin,
		UnikernelType:       "t",
		Hypervisor:          "firecracker",
		FCBinaryPath:        "/fc",
		FCVersion:           "v1",
		Arch:                "amd64",
		MemMiB:              256,
		VCPUs:               1,
		GuestCmdLine:        "/u",
	}
	k0, _, err := ComputeKey(base)
	if err != nil {
		t.Fatal(err)
	}

	// Each mutation should flip the key.
	muts := []struct {
		name string
		fn   func(*KeyInputs)
	}{
		{"mem", func(k *KeyInputs) { k.MemMiB = 512 }},
		{"vcpus", func(k *KeyInputs) { k.VCPUs = 2 }},
		{"fcver", func(k *KeyInputs) { k.FCVersion = "v2" }},
		{"arch", func(k *KeyInputs) { k.Arch = "arm64" }},
		{"cmd", func(k *KeyInputs) { k.GuestCmdLine = "/u -x" }},
		{"type", func(k *KeyInputs) { k.UnikernelType = "other" }},
	}
	for _, m := range muts {
		kin := base
		m.fn(&kin)
		k, _, err := ComputeKey(kin)
		if err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		if k == k0 {
			t.Fatalf("%s: key did not change", m.name)
		}
	}
}

func TestComputeKeyChangesWithBinaryContent(t *testing.T) {
	tmp := t.TempDir()
	bin1 := writeTempFile(t, tmp, "u1", []byte("aaa"))
	bin2 := writeTempFile(t, tmp, "u2", []byte("bbb"))

	in := KeyInputs{
		UnikernelBinaryPath: bin1,
		UnikernelType:       "t",
		Hypervisor:          "firecracker",
		FCBinaryPath:        "/fc",
		FCVersion:           "v",
		Arch:                "amd64",
		MemMiB:              1,
		VCPUs:               1,
	}
	k1, _, err := ComputeKey(in)
	if err != nil {
		t.Fatal(err)
	}
	in.UnikernelBinaryPath = bin2
	k2, _, err := ComputeKey(in)
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatalf("key unchanged with different binary")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		CacheKey:     "key",
		GuestMAC:     "AA:FC:00:11:22:33",
		IfaceID:      "net1",
		MemMiB:       256,
		VCPUs:        1,
		FCVersion:    "sha",
		Arch:         "amd64",
		CreatedAt:    time.Now().UTC().Round(time.Second),
		UnikernelBin: "deadbeef",
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.CacheKey != m.CacheKey || got.GuestMAC != m.GuestMAC || got.MemMiB != m.MemMiB {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, m)
	}
	if got.Version != currentManifestVersion {
		t.Fatalf("manifest version not set: got %d", got.Version)
	}
}

func TestManifestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{CacheKey: "k"}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("write: %v", err)
	}
	// No leftover .tmp file after successful rename.
	if _, err := os.Stat(filepath.Join(dir, ManifestFilename+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp file should be gone after rename: %v", err)
	}
}

func TestCacheEntryCreateDir(t *testing.T) {
	root := t.TempDir()
	c := NewCache(filepath.Join(root, "snapshots"))
	e, err := c.Entry("abc")
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	if _, err := os.Stat(e.Dir); err != nil {
		t.Fatalf("cache dir should exist: %v", err)
	}
	if e.HasTemplate() {
		t.Fatalf("empty entry should not have template")
	}
}

func TestCacheEntryCommitBuild(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	e, err := c.Entry("k1")
	if err != nil {
		t.Fatal(err)
	}
	vmstateTmp, memTmp := e.TempPaths()
	if err := os.WriteFile(vmstateTmp, []byte("vmstate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memTmp, []byte("mem"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{CacheKey: "k1", GuestMAC: "AA:FC:00:00:00:01"}
	if err := e.CommitBuild(m); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !e.HasTemplate() {
		t.Fatalf("HasTemplate should be true after commit")
	}
	// .tmp files should be gone.
	for _, p := range []string{vmstateTmp, memTmp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should not exist after commit: %v", p, err)
		}
	}
	// Committed files should be readable.
	for _, p := range []string{e.Vmstate, e.Mem} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing: %v", p, err)
		}
	}
}

// TestCacheLockSerializes starts N goroutines that all try to AcquireLock.
// Only one should hold the lock at any instant; total wall time should be
// at least N * holdDur.
func TestCacheLockSerializes(t *testing.T) {
	root := t.TempDir()
	c := NewCache(root)
	e, err := c.Entry("locky")
	if err != nil {
		t.Fatal(err)
	}

	const n = 4
	const holdDur = 50 * time.Millisecond
	var held atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := e.AcquireLock()
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			h := held.Add(1)
			if h != 1 {
				t.Errorf("lock violated: %d holders", h)
			}
			time.Sleep(holdDur)
			held.Add(-1)
			_ = l.Close()
		}()
	}

	start := time.Now()
	wg.Wait()
	elapsed := time.Since(start)
	if elapsed < time.Duration(n-1)*holdDur {
		t.Fatalf("elapsed %v too short, lock did not serialize %d workers", elapsed, n)
	}
}
