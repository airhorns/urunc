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

package unikontainers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/urunc-dev/urunc/pkg/unikontainers/snapshot"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// snapshotEnabled reports whether the container opted into the firecracker
// snapshot/fork path via annotation. Accepts "true"/"1"/"yes" (case-insensitive).
func snapshotEnabled(annotations map[string]string) bool {
	v := strings.ToLower(strings.TrimSpace(annotations[annotSnapshotEnable]))
	switch v {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// snapshotReadyDelayMs parses the opt-in delay before snapshot/create. Returns
// 0 if unset so that firecracker.Supervise can apply its default.
func snapshotReadyDelayMs(annotations map[string]string) uint {
	v := strings.TrimSpace(annotations[annotSnapshotReadyDelayMs])
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0
	}
	return uint(n)
}

// snapshotPrep is the outcome of resolving a snapshot cache entry for a run.
// The caller must ensure lock (if non-nil) is eventually released. On a build,
// that happens inside SnapshotArgs.CommitFn; on a restore, it is released
// before prepareSnapshot returns.
type snapshotPrep struct {
	args  *types.SnapshotArgs
	cache *snapshot.Entry
}

// prepareSnapshot computes the cache key for this run, takes the per-entry
// flock, and decides build-vs-restore based on manifest presence.
//
// hostUnikernelPath and hostInitrdPath are absolute paths on the host (already
// resolved against monRootfs). fcBinaryPath is the firecracker binary path.
func (u *Unikontainer) prepareSnapshot(
	vmmArgs types.ExecArgs,
	unikernelType string,
	unikernelVersion string,
	hypervisor string,
	hostUnikernelPath string,
	hostInitrdPath string,
	fcBinaryPath string,
	guestCmdLine string,
) (*snapshotPrep, error) {
	fcSha, err := sha256File(fcBinaryPath)
	if err != nil {
		return nil, fmt.Errorf("hash firecracker binary: %w", err)
	}

	cacheKey, unikSha, err := snapshot.ComputeKey(snapshot.KeyInputs{
		UnikernelBinaryPath: hostUnikernelPath,
		InitrdPath:          hostInitrdPath,
		UnikernelType:       unikernelType,
		UnikernelVersion:    unikernelVersion,
		Hypervisor:          hypervisor,
		FCBinaryPath:        fcBinaryPath,
		FCVersion:           fcSha,
		Arch:                runtime.GOARCH,
		MemMiB:              vmmArgs.MemSizeB / (1024 * 1024),
		VCPUs:               vmmArgs.VCPUs,
		GuestCmdLine:        guestCmdLine,
	})
	if err != nil {
		return nil, err
	}

	cache := snapshot.NewCache("")
	entry, err := cache.Entry(cacheKey)
	if err != nil {
		return nil, err
	}

	lock, err := entry.AcquireLock()
	if err != nil {
		return nil, err
	}

	// The fc API socket path must fit inside sun_path (108 bytes on Linux).
	// Both the cache dir and container ID are 64-char sha256 strings, so
	// joining them blows the limit. Keep the socket at a short /run path
	// keyed by a short prefix of the container ID.
	cidPrefix := vmmArgs.ContainerID
	if len(cidPrefix) > 12 {
		cidPrefix = cidPrefix[:12]
	}
	sa := &types.SnapshotArgs{
		CacheDir:      entry.Dir,
		APISocketPath: filepath.Join("/run", "urunc-fc-"+cidPrefix+".sock"),
		IfaceID:       "net1",
		ReadyDelayMs:  snapshotReadyDelayMs(u.State.Annotations),
	}

	if entry.HasTemplate() {
		m, mErr := snapshot.ReadManifest(entry.Dir)
		if mErr != nil {
			_ = lock.Close()
			return nil, fmt.Errorf("read manifest: %w", mErr)
		}
		sa.CacheHit = true
		sa.GuestMAC = m.GuestMAC
		sa.VmstatePath = entry.Vmstate
		sa.MemPath = entry.Mem
		// Restore doesn't need to serialize against anyone — release now.
		_ = lock.Close()
		return &snapshotPrep{args: sa, cache: entry}, nil
	}

	// Build path.
	sa.CacheHit = false
	sa.GuestMAC = templateMAC(cacheKey)
	vmstateTmp, memTmp := entry.TempPaths()
	sa.VmstatePath = vmstateTmp
	sa.MemPath = memTmp

	heldLock := lock
	capturedEntry := entry
	capturedKey := cacheKey
	capturedSha := unikSha
	memMiB := vmmArgs.MemSizeB / (1024 * 1024)
	vcpus := vmmArgs.VCPUs
	sa.CommitFn = func() error {
		defer func() { _ = heldLock.Close() }()
		return capturedEntry.CommitBuild(&snapshot.Manifest{
			CacheKey:     capturedKey,
			GuestMAC:     sa.GuestMAC,
			IfaceID:      sa.IfaceID,
			MemMiB:       memMiB,
			VCPUs:        vcpus,
			FCVersion:    fcSha,
			Arch:         runtime.GOARCH,
			CreatedAt:    time.Now().UTC(),
			UnikernelBin: capturedSha,
		})
	}

	return &snapshotPrep{args: sa, cache: entry}, nil
}

// templateMAC derives a deterministic MAC for a template from the cache key.
// Prefix AA:FC keeps it in the locally-administered unicast range and makes it
// grep-able in logs. The remaining four bytes come from the first 8 hex chars
// of the key, which is already a sha256.
func templateMAC(cacheKey string) string {
	if len(cacheKey) < 8 {
		return "AA:FC:00:00:00:01"
	}
	return fmt.Sprintf(
		"AA:FC:%s:%s:%s:%s",
		cacheKey[0:2],
		cacheKey[2:4],
		cacheKey[4:6],
		cacheKey[6:8],
	)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
