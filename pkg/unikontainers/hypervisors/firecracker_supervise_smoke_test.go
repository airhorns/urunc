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

//go:build linux && fcsmoke

package hypervisors

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// Stub Unikernel — the smoke test drives firecracker directly with a real
// kernel; the Unikernel interface only exists because Supervise consumes it.
type smokeUK struct {
	rootfs string
}

func (u *smokeUK) Init(types.UnikernelParams) error     { return nil }
func (u *smokeUK) CommandString() (string, error)       { return "", nil }
func (u *smokeUK) SupportsBlock() bool                  { return u.rootfs != "" }
func (u *smokeUK) SupportsFS(string) bool               { return false }
func (u *smokeUK) MonitorNetCli(string, string) string  { return "" }
func (u *smokeUK) MonitorCli() types.MonitorCliArgs     { return types.MonitorCliArgs{} }
func (u *smokeUK) MonitorBlockCli() []types.MonitorBlockArgs {
	if u.rootfs == "" {
		return nil
	}
	return []types.MonitorBlockArgs{{ID: "rootfs", Path: u.rootfs}}
}

func requireEnv(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("smoke test needs %s", k)
	}
	return v
}

// TestSuperviseSmokeBuildAndRestore spawns real firecracker, drives it
// through both the snapshot-build and snapshot-restore paths of Supervise,
// and verifies the cache files end up on disk.
//
// Requirements (set via env):
//   FCSMOKE_KERNEL=/path/to/vmlinux   (required)
//   FCSMOKE_ROOTFS=/path/to/rootfs    (optional squashfs/ext4; else no drive)
//
// Run: sudo -E go test -tags fcsmoke -run TestSuperviseSmoke \
//        ./pkg/unikontainers/hypervisors/...
//
// Requires /dev/kvm. Must run as root so firecracker can open KVM.
func TestSuperviseSmokeBuildAndRestore(t *testing.T) {
	kernel := requireEnv(t, "FCSMOKE_KERNEL")
	rootfs := os.Getenv("FCSMOKE_ROOTFS")

	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm missing: %v", err)
	}

	vmm, err := NewVMM(FirecrackerVmm, nil)
	if err != nil {
		t.Fatalf("NewVMM: %v", err)
	}
	fc, ok := vmm.(*Firecracker)
	if !ok {
		t.Fatalf("vmm is not Firecracker: %T", vmm)
	}
	snap, ok := vmm.(types.SnapshotVMM)
	if !ok {
		t.Fatalf("firecracker does not implement SnapshotVMM")
	}
	_ = fc

	cacheDir := t.TempDir()
	vmstate := filepath.Join(cacheDir, "vmstate")
	mem := filepath.Join(cacheDir, "mem")
	sock := filepath.Join(cacheDir, "fc.sock")

	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off"
	if rootfs != "" {
		bootArgs += " root=/dev/vda ro"
	}

	// ------- build path -------
	commitCalled := false
	buildArgs := types.ExecArgs{
		ContainerID:   "fcsmoke-build",
		Environment:   os.Environ(),
		Command:       bootArgs,
		Seccomp:       false,
		MemSizeB:      128 * 1024 * 1024,
		VCPUs:         1,
		UnikernelPath: kernel,
		Snapshot: &types.SnapshotArgs{
			CacheDir:      cacheDir,
			VmstatePath:   vmstate,
			MemPath:       mem,
			CacheHit:      false,
			GuestMAC:      "AA:FC:00:00:00:01",
			IfaceID:       "net1",
			ReadyDelayMs:  1500,
			APISocketPath: sock,
			CommitFn: func() error {
				commitCalled = true
				return nil
			},
		},
	}

	uk := &smokeUK{rootfs: rootfs}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- snap.Supervise(buildArgs, uk)
	}()

	// Let fc build the snapshot and continue running. Then kill so Supervise returns.
	select {
	case err := <-errCh:
		t.Fatalf("Supervise returned too early: %v", err)
	case <-time.After(5 * time.Second):
	}

	// Signal ourselves — the Supervise goroutine installed a SIGTERM forwarder
	// to the fc child process group. But that handler is in our process too,
	// so sending SIGTERM would kill the test. Instead, kill fc directly by
	// locating its pid via the API socket's process. Easier: send SIGTERM to
	// the entire process group? We're the test process. Use pgrep.
	killFirecracker(t)

	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("Supervise returned (expected on child kill): %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("Supervise did not return after fc kill")
	}

	// Verify commit + files
	if !commitCalled {
		t.Fatalf("CommitFn not called on build path")
	}
	for _, p := range []string{vmstate, mem} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("snapshot file %s missing: %v", p, err)
		}
		if st.Size() == 0 {
			t.Fatalf("snapshot file %s is empty", p)
		}
		t.Logf("snapshot file %s size=%d", p, st.Size())
	}

	// ------- restore path -------
	sock2 := filepath.Join(cacheDir, "fc2.sock")
	restoreArgs := buildArgs
	restoreArgs.ContainerID = "fcsmoke-restore"
	sn2 := *buildArgs.Snapshot
	sn2.CacheHit = true
	sn2.APISocketPath = sock2
	sn2.CommitFn = nil
	restoreArgs.Snapshot = &sn2

	errCh2 := make(chan error, 1)
	go func() {
		errCh2 <- snap.Supervise(restoreArgs, uk)
	}()

	select {
	case err := <-errCh2:
		t.Fatalf("Supervise (restore) returned too early: %v", err)
	case <-time.After(4 * time.Second):
	}

	killFirecracker(t)

	select {
	case err := <-errCh2:
		if err != nil {
			t.Logf("Supervise restore returned (expected on child kill): %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("Supervise restore did not return")
	}
	t.Log("restore path completed")
}

// killFirecracker sends SIGKILL to every firecracker process owned by the
// current uid. Safe in a smoke-test harness: we only spawn fc ourselves.
func killFirecracker(t *testing.T) {
	t.Helper()
	// ignore the goroutine signal handler — it installs its own Notify
	// channel on SIGTERM/SIGINT; we kill fc directly by scanning /proc.
	procs, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	uid := os.Getuid()
	var killed int
	for _, d := range procs {
		if !d.IsDir() {
			continue
		}
		pid := 0
		for _, c := range d.Name() {
			if c < '0' || c > '9' {
				pid = 0
				break
			}
			pid = pid*10 + int(c-'0')
		}
		if pid == 0 {
			continue
		}
		commBytes, err := os.ReadFile("/proc/" + d.Name() + "/comm")
		if err != nil {
			continue
		}
		if string(commBytes) != "firecracker\n" {
			continue
		}
		var st syscall.Stat_t
		if err := syscall.Stat("/proc/"+d.Name(), &st); err != nil {
			continue
		}
		if int(st.Uid) != uid {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed++
		}
	}
	if killed == 0 {
		t.Fatalf("no firecracker process found to kill")
	}
	t.Logf("killed %d firecracker process(es)", killed)
}
