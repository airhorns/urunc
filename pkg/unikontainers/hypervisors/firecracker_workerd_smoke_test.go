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

//go:build linux && fcworkerd

package hypervisors

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// workerdUK mirrors what a real underbelly-worker urunc container would
// expose: an initrd path (MonitorCli.ExtraInitrd) and no block device.
type workerdUK struct{ initrd string }

func (u *workerdUK) Init(types.UnikernelParams) error        { return nil }
func (u *workerdUK) CommandString() (string, error)          { return "", nil }
func (u *workerdUK) SupportsBlock() bool                     { return false }
func (u *workerdUK) SupportsFS(string) bool                  { return false }
func (u *workerdUK) MonitorNetCli(string, string) string     { return "" }
func (u *workerdUK) MonitorBlockCli() []types.MonitorBlockArgs { return nil }
func (u *workerdUK) MonitorCli() types.MonitorCliArgs {
	return types.MonitorCliArgs{ExtraInitrd: u.initrd}
}

const (
	wkTapName   = "tapwk0"
	wkHostCIDR  = "172.44.0.1/24"
	wkGuestIP   = "172.44.0.2"
	wkGuestCIDR = "172.44.0.2/24"
	wkGuestGW   = "172.44.0.1"
	wkGuestPort = "8080"
	wkGuestMAC  = "AA:FC:00:44:00:02"
)

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// setupTap creates a fresh tap device with a host-side IP and starts a
// dnsmasq DHCP server bound to it. LWIP in this unikernel does DHCP
// rather than honoring the static netdev.ip= cmdline param.
func setupTap(t *testing.T) {
	t.Helper()
	_ = exec.Command("ip", "link", "del", wkTapName).Run()
	mustRun(t, "ip", "tuntap", "add", "dev", wkTapName, "mode", "tap")
	mustRun(t, "ip", "addr", "add", wkHostCIDR, "dev", wkTapName)
	mustRun(t, "ip", "link", "set", wkTapName, "up")

	pidFile := filepath.Join(t.TempDir(), "dnsmasq.pid")
	leaseFile := filepath.Join(t.TempDir(), "dnsmasq.leases")
	dnsCmd := exec.Command("/usr/sbin/dnsmasq",
		"--interface="+wkTapName,
		"--bind-interfaces",
		"--except-interface=lo",
		"--no-resolv",
		"--no-hosts",
		"--dhcp-range="+wkGuestIP+","+wkGuestIP+",12h",
		"--dhcp-host="+wkGuestMAC+","+wkGuestIP,
		"--pid-file="+pidFile,
		"--dhcp-leasefile="+leaseFile,
		"--log-dhcp",
		"--log-facility=-",
	)
	dnsCmd.Stdout = os.Stderr
	dnsCmd.Stderr = os.Stderr
	if err := dnsCmd.Start(); err != nil {
		t.Fatalf("start dnsmasq: %v", err)
	}

	t.Cleanup(func() {
		if dnsCmd.Process != nil {
			_ = dnsCmd.Process.Signal(syscall.SIGTERM)
			_ = dnsCmd.Wait()
		}
		_ = exec.Command("ip", "link", "del", wkTapName).Run()
	})
}

// waitForHTTP polls an HTTP endpoint until it returns any response or the
// deadline passes. Returns the final status and body.
func waitForHTTP(t *testing.T, url string, deadline time.Duration) (int, string) {
	t.Helper()
	end := time.Now().Add(deadline)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	var lastErr error
	for time.Now().Before(end) {
		resp, err := client.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp.StatusCode, string(body)
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("HTTP %s never responded within %v: last err %v", url, deadline, lastErr)
	return 0, ""
}

// portProbe returns nil once a TCP dial to addr succeeds.
func portProbe(addr string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}
}

// bootArgsForWorkerd builds the Unikraft cmdline with static IP config
// plus the workerd application argv after the "--" separator.
func bootArgsForWorkerd() string {
	// Unikraft netdev static IP: cidr[:gw[:dns0[:dns1[:hostname[:domain]]]]]
	return "netdev.ip=" + wkGuestCIDR + ":" + wkGuestGW + ":1.1.1.1:8.8.8.8:worker:local" +
		" -- /usr/bin/workerd serve --experimental /etc/workerd/workerd.capnp"
}

// killAllFirecracker best-effort SIGKILLs every fc process owned by this
// uid. Used between build/restore/restore to return Supervise.
func killAllFirecracker(t *testing.T) {
	t.Helper()
	procs, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	uid := os.Getuid()
	killed := 0
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
		comm, err := os.ReadFile("/proc/" + d.Name() + "/comm")
		if err != nil || string(comm) != "firecracker\n" {
			continue
		}
		var st syscall.Stat_t
		if err := syscall.Stat("/proc/"+d.Name(), &st); err != nil || int(st.Uid) != uid {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed++
		}
	}
	t.Logf("killed %d firecracker process(es)", killed)
}

// TestSuperviseSmokeWorkerdSnapshotAndRestore boots the real underbelly-worker
// unikernel under firecracker, builds a snapshot mid-run, then restarts
// two additional copies from that snapshot, verifying /healthz on each.
//
// Requirements:
//   FCWK_KERNEL=/path/to/underbelly-worker_fc-x86_64
//   FCWK_INITRD=/path/to/initramfs-x86_64.cpio
//   sudo -E (tap + fc need CAP_NET_ADMIN / KVM)
//
// Run:
//   sudo -E go test -tags fcworkerd -v -run TestSuperviseSmokeWorkerd \
//        -timeout 240s ./pkg/unikontainers/hypervisors/...
func TestSuperviseSmokeWorkerdSnapshotAndRestore(t *testing.T) {
	kernel := os.Getenv("FCWK_KERNEL")
	initrd := os.Getenv("FCWK_INITRD")
	if kernel == "" || initrd == "" {
		t.Skip("FCWK_KERNEL + FCWK_INITRD required")
	}
	for _, p := range []string{kernel, initrd} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm missing: %v", err)
	}

	setupTap(t)

	vmm, err := NewVMM(FirecrackerVmm, nil)
	if err != nil {
		t.Fatalf("NewVMM: %v", err)
	}
	snap := vmm.(types.SnapshotVMM)

	cacheDir := t.TempDir()
	vmstate := filepath.Join(cacheDir, "vmstate")
	memFile := filepath.Join(cacheDir, "mem")

	uk := &workerdUK{initrd: initrd}
	healthzURL := "http://" + wkGuestIP + ":" + wkGuestPort + "/healthz"

	baseArgs := types.ExecArgs{
		Environment:   os.Environ(),
		Command:       bootArgsForWorkerd(),
		Seccomp:       false,
		MemSizeB:      1024 * 1024 * 1024, // 1 GiB — workerd needs headroom
		VCPUs:         2,
		UnikernelPath: kernel,
		Net: types.NetDevParams{
			TapDev: wkTapName,
			MAC:    wkGuestMAC,
		},
	}

	// ------- BUILD -------
	committed := make(chan struct{}, 1)
	buildArgs := baseArgs
	buildArgs.ContainerID = "workerd-build"
	buildArgs.Snapshot = &types.SnapshotArgs{
		CacheDir:      cacheDir,
		VmstatePath:   vmstate,
		MemPath:       memFile,
		CacheHit:      false,
		GuestMAC:      wkGuestMAC,
		IfaceID:       "net1",
		ReadyDelayMs:  20000, // workerd takes several seconds to come up
		APISocketPath: filepath.Join(cacheDir, "fc-build.sock"),
		CommitFn: func() error {
			committed <- struct{}{}
			return nil
		},
	}

	buildDone := make(chan error, 1)
	go func() { buildDone <- snap.Supervise(buildArgs, uk) }()

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Wait for guest to be reachable on the host tap first (fast probe),
	// then hit /healthz. The build path holds the VM open past snapshot,
	// so /healthz should be live both before and after snapshot/create.
	if err := portProbe(wkGuestIP+":"+wkGuestPort, 60*time.Second); err != nil {
		t.Fatalf("guest port did not open: %v", err)
	}
	code, body := waitForHTTP(t, healthzURL, 30*time.Second)
	t.Logf("build /healthz (pre-snapshot) → %d %q", code, body)
	if code != 200 {
		t.Fatalf("build /healthz expected 200, got %d body=%q", code, body)
	}

	// Wait for Supervise.buildTemplate to finish snapshot/create + resume
	// and invoke CommitFn. Only then can we kill fc.
	select {
	case <-committed:
		t.Logf("snapshot CommitFn fired")
	case err := <-buildDone:
		t.Fatalf("build Supervise returned before CommitFn: %v", err)
	case <-ctx.Done():
		t.Fatal("CommitFn never fired")
	}

	// /healthz should still be live post-resume (snapshot builds on a
	// resumed VM, so the original container continues serving).
	code, body = waitForHTTP(t, healthzURL, 10*time.Second)
	t.Logf("build /healthz (post-snapshot) → %d %q", code, body)
	if code != 200 {
		t.Fatalf("build post-snapshot /healthz expected 200, got %d", code)
	}

	killAllFirecracker(t)
	select {
	case err := <-buildDone:
		t.Logf("build Supervise returned: %v", err)
	case <-ctx.Done():
		t.Fatal("build Supervise did not return after kill")
	}

	for _, p := range []string{vmstate, memFile} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("snapshot file %s missing: %v", p, err)
		}
		t.Logf("snapshot %s size=%d", p, st.Size())
	}

	// ------- RESTORE x2 -------
	for i := 1; i <= 2; i++ {
		sock := filepath.Join(cacheDir, "fc-restore-" + string(rune('0'+i)) + ".sock")
		restoreArgs := baseArgs
		restoreArgs.ContainerID = "workerd-restore-" + string(rune('0'+i))
		sn := *buildArgs.Snapshot
		sn.CacheHit = true
		sn.APISocketPath = sock
		sn.CommitFn = nil
		restoreArgs.Snapshot = &sn

		done := make(chan error, 1)
		go func() { done <- snap.Supervise(restoreArgs, uk) }()

		if err := portProbe(wkGuestIP+":"+wkGuestPort, 30*time.Second); err != nil {
			t.Fatalf("restore %d: port did not open: %v", i, err)
		}
		code, body := waitForHTTP(t, healthzURL, 20*time.Second)
		t.Logf("restore %d /healthz → %d %q", i, code, body)
		if code != 200 {
			t.Fatalf("restore %d /healthz expected 200, got %d body=%q", i, code, body)
		}

		killAllFirecracker(t)
		select {
		case err := <-done:
			t.Logf("restore %d Supervise returned: %v", i, err)
		case <-ctx.Done():
			t.Fatalf("restore %d Supervise did not return after kill", i)
		}
	}
}
