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

//go:build linux

package hypervisors

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// Firecracker API request bodies used only on the supervised snapshot path.
type fcMachineConfigReq struct {
	VcpuCount       uint   `json:"vcpu_count"`
	MemSizeMiB      uint64 `json:"mem_size_mib"`
	Smt             bool   `json:"smt"`
	TrackDirtyPages bool   `json:"track_dirty_pages"`
}

type fcBootSourceReq struct {
	ImagePath  string `json:"kernel_image_path"`
	BootArgs   string `json:"boot_args"`
	InitrdPath string `json:"initrd_path,omitempty"`
}

type fcNetIfaceReq struct {
	IfaceID  string `json:"iface_id"`
	GuestMAC string `json:"guest_mac,omitempty"`
	HostIF   string `json:"host_dev_name"`
}

type fcDriveReq struct {
	DriveID   string `json:"drive_id"`
	IsRO      bool   `json:"is_read_only"`
	IsRootDev bool   `json:"is_root_device"`
	HostPath  string `json:"path_on_host"`
}

type fcActionReq struct {
	ActionType string `json:"action_type"`
}

type fcVMStateReq struct {
	State string `json:"state"`
}

type fcSnapshotCreateReq struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

type fcMemBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

type fcNetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

type fcSnapshotLoadReq struct {
	SnapshotPath     string              `json:"snapshot_path"`
	MemBackend       fcMemBackend        `json:"mem_backend"`
	NetworkOverrides []fcNetworkOverride `json:"network_overrides,omitempty"`
	ResumeVM         bool                `json:"resume_vm"`
}

// Supervise implements types.SnapshotVMM. It spawns firecracker under the
// monitor's API socket, either creates a template snapshot (when
// args.Snapshot.CacheHit is false) or loads one (when true), then blocks
// until the firecracker child exits.
//
// On kill (the supervisor process receiving a signal from the outer urunc
// lifecycle), the firecracker child is killed via its process group and
// Pdeathsig. The function returns when the child has exited.
func (fc *Firecracker) Supervise(args types.ExecArgs, ukernel types.Unikernel) error {
	sn := args.Snapshot
	if sn == nil {
		return fmt.Errorf("firecracker.Supervise: nil SnapshotArgs")
	}

	_ = os.Remove(sn.APISocketPath)

	fcArgs := []string{
		"--api-sock", sn.APISocketPath,
		"--id", args.ContainerID,
	}
	if !args.Seccomp {
		fcArgs = append(fcArgs, "--no-seccomp")
	}

	vmmLog.WithField("args", fcArgs).Debug("spawning firecracker (supervised)")
	cmd := exec.Command(fc.Path(), fcArgs...)
	cmd.Env = args.Environment
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}

	client := newFCAPIClient(sn.APISocketPath)
	if err := client.waitForSocket(5 * time.Second); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		for s := range sigCh {
			if cmd.Process == nil {
				return
			}
			_ = syscall.Kill(-cmd.Process.Pid, s.(syscall.Signal))
		}
	}()

	var configErr error
	if sn.CacheHit {
		configErr = fc.restoreFromSnapshot(client, args, sn)
	} else {
		configErr = fc.buildTemplate(client, args, ukernel, sn)
	}
	if configErr != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = os.Remove(sn.APISocketPath)
		return configErr
	}

	err := cmd.Wait()
	_ = os.Remove(sn.APISocketPath)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() >= 0 {
			vmmLog.WithError(err).Debug("firecracker exited")
		} else {
			return fmt.Errorf("firecracker wait: %w", err)
		}
	}
	return nil
}

func (fc *Firecracker) restoreFromSnapshot(c *fcAPIClient, args types.ExecArgs, sn *types.SnapshotArgs) error {
	overrides := []fcNetworkOverride{}
	if args.Net.TapDev != "" {
		overrides = append(overrides, fcNetworkOverride{
			IfaceID:     sn.IfaceID,
			HostDevName: args.Net.TapDev,
		})
	}
	req := &fcSnapshotLoadReq{
		SnapshotPath: sn.VmstatePath,
		MemBackend: fcMemBackend{
			BackendType: "File",
			BackendPath: sn.MemPath,
		},
		NetworkOverrides: overrides,
		ResumeVM:         true,
	}
	vmmLog.WithField("req", req).Debug("firecracker snapshot/load")
	if err := c.Put("/snapshot/load", req); err != nil {
		return fmt.Errorf("PUT /snapshot/load: %w", err)
	}
	return nil
}

func (fc *Firecracker) buildTemplate(c *fcAPIClient, args types.ExecArgs, ukernel types.Unikernel, sn *types.SnapshotArgs) error {
	cfg := buildFirecrackerConfig(args, ukernel, sn.GuestMAC)

	if err := c.Put("/machine-config", &fcMachineConfigReq{
		VcpuCount:       cfg.Machine.VcpuCount,
		MemSizeMiB:      cfg.Machine.MemSizeMiB,
		Smt:             cfg.Machine.Smt,
		TrackDirtyPages: cfg.Machine.TrackDirtyPages,
	}); err != nil {
		return fmt.Errorf("PUT /machine-config: %w", err)
	}

	if err := c.Put("/boot-source", &fcBootSourceReq{
		ImagePath:  cfg.Source.ImagePath,
		BootArgs:   cfg.Source.BootArgs,
		InitrdPath: cfg.Source.InitrdPath,
	}); err != nil {
		return fmt.Errorf("PUT /boot-source: %w", err)
	}

	for _, d := range cfg.Drives {
		if err := c.Put("/drives/"+d.DriveID, &fcDriveReq{
			DriveID:   d.DriveID,
			IsRO:      d.IsRO,
			IsRootDev: d.IsRootDev,
			HostPath:  d.HostPath,
		}); err != nil {
			return fmt.Errorf("PUT /drives/%s: %w", d.DriveID, err)
		}
	}

	for _, n := range cfg.NetIfs {
		if err := c.Put("/network-interfaces/"+n.IfaceID, &fcNetIfaceReq{
			IfaceID:  n.IfaceID,
			GuestMAC: n.GuestMAC,
			HostIF:   n.HostIF,
		}); err != nil {
			return fmt.Errorf("PUT /network-interfaces/%s: %w", n.IfaceID, err)
		}
	}

	if err := c.Put("/actions", &fcActionReq{ActionType: "InstanceStart"}); err != nil {
		return fmt.Errorf("PUT /actions InstanceStart: %w", err)
	}

	delay := time.Duration(sn.ReadyDelayMs) * time.Millisecond
	if delay <= 0 {
		delay = 2 * time.Second
	}
	vmmLog.WithField("delay", delay).Debug("sleeping before snapshot/create")
	time.Sleep(delay)

	if err := c.Patch("/vm", &fcVMStateReq{State: "Paused"}); err != nil {
		return fmt.Errorf("PATCH /vm Paused: %w", err)
	}

	if err := c.Put("/snapshot/create", &fcSnapshotCreateReq{
		SnapshotType: "Full",
		SnapshotPath: sn.VmstatePath,
		MemFilePath:  sn.MemPath,
	}); err != nil {
		return fmt.Errorf("PUT /snapshot/create: %w", err)
	}

	if err := c.Patch("/vm", &fcVMStateReq{State: "Resumed"}); err != nil {
		return fmt.Errorf("PATCH /vm Resumed: %w", err)
	}

	if sn.CommitFn != nil {
		if err := sn.CommitFn(); err != nil {
			return fmt.Errorf("commit template cache: %w", err)
		}
	}
	return nil
}
