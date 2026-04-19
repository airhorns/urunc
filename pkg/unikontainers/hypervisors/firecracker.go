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

package hypervisors

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

const (
	FirecrackerVmm    VmmType = "firecracker"
	FirecrackerBinary string  = "firecracker"
	FCJsonFilename    string  = "fc.json"
)

type Firecracker struct {
	binaryPath string
	binary     string
}

type FirecrackerBootSource struct {
	ImagePath  string `json:"kernel_image_path"`
	BootArgs   string `json:"boot_args"`
	InitrdPath string `json:"initrd_path,omitempty"`
}

type FirecrackerMachine struct {
	VcpuCount       uint   `json:"vcpu_count"`
	MemSizeMiB      uint64 `json:"mem_size_mib"`
	Smt             bool   `json:"smt"`
	TrackDirtyPages bool   `json:"track_dirty_pages"`
}

type FirecrackerDrive struct {
	DriveID   string `json:"drive_id"`
	IsRO      bool   `json:"is_read_only"`
	IsRootDev bool   `json:"is_root_device"`
	HostPath  string `json:"path_on_host"`
}

type FirecrackerNet struct {
	IfaceID  string `json:"iface_id"`
	GuestMAC string `json:"guest_mac,omitempty"`
	HostIF   string `json:"host_dev_name"`
}

type FirecrackerVSockDev struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
	VSockID  string `json:"vsock_id"`
}

type FirecrackerConfig struct {
	Source  FirecrackerBootSource `json:"boot-source"`
	Machine FirecrackerMachine    `json:"machine-config"`
	Drives  []FirecrackerDrive    `json:"drives"`
	NetIfs  []FirecrackerNet      `json:"network-interfaces,omitempty"`
	VSock   FirecrackerVSockDev   `json:"vsock,omitempty"`
}

func (fc *Firecracker) Stop(pid int) error {
	return killProcess(pid)
}

func (fc *Firecracker) Ok() error {
	return nil
}

func (fc *Firecracker) UsesKVM() bool {
	return true
}

// SupportsSharedfs returns a bool value depending on the monitor support for shared-fs
func (fc *Firecracker) SupportsSharedfs(_ string) bool {
	return false
}

func (fc *Firecracker) Path() string {
	return fc.binaryPath
}

func (fc *Firecracker) BuildExecCmd(args types.ExecArgs, ukernel types.Unikernel) ([]string, error) {
	// FIXME: Note for getting unikernel specific options.
	// Due to the way FC operates, we have not encountered any guest specific
	// options yet. However, we need to revisit how we can use guest specific
	// options in FC, since the string return value of the Monitor related
	// functions in the unikernel interface do not integrate well with FC's
	// json configuration.
	cmdString := fc.Path() + " --no-api --config-file "
	JSONConfigFile := filepath.Join("/tmp/", FCJsonFilename)
	cmdString += JSONConfigFile
	if !args.Seccomp {
		cmdString += " --no-seccomp"
	}

	FCConfig := buildFirecrackerConfig(args, ukernel, args.Net.MAC)

	FCConfigJSON, err := json.Marshal(FCConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Firecracker config: %w", err)
	}
	if err = os.WriteFile(JSONConfigFile, FCConfigJSON, 0o644); err != nil { //nolint: gosec
		return nil, fmt.Errorf("failed to save Firecracker json config: %w", err)
	}
	vmmLog.WithField("Json", string(FCConfigJSON)).Debug("Firecracker json config")

	exArgs := strings.Split(cmdString, " ")
	return exArgs, nil
}

// buildFirecrackerConfig translates urunc ExecArgs + Unikernel hooks into
// the set of firecracker configuration structs that together describe a
// cold-boot VM. Used by both the --config-file path (BuildExecCmd) and the
// API-driven build path (Supervise).
//
// guestMAC overrides args.Net.MAC: callers on the snapshot-build path pass
// the deterministic template MAC; callers on the cold-boot path pass the
// veth-derived MAC (same as args.Net.MAC).
func buildFirecrackerConfig(args types.ExecArgs, ukernel types.Unikernel, guestMAC string) *FirecrackerConfig {
	fcMem := DefaultMemory
	if args.MemSizeB != 0 {
		fcMem = bytesToMiB(args.MemSizeB)
		if fcMem == 0 {
			fcMem = DefaultMemory
		}
	}

	// NOTE: Firecracker supports only one initrd. Concatenation of multiple
	// initrds is handled by the unikernel implementation; hence prefer the
	// path from args when set.
	extraMonArgs := ukernel.MonitorCli()
	initrdPath := args.InitrdPath
	if initrdPath == "" {
		initrdPath = extraMonArgs.ExtraInitrd
	}

	machine := FirecrackerMachine{
		VcpuCount:       args.VCPUs,
		MemSizeMiB:      fcMem,
		Smt:             false,
		TrackDirtyPages: false,
	}

	nets := make([]FirecrackerNet, 0)
	if args.Net.TapDev != "" {
		nets = append(nets, FirecrackerNet{
			IfaceID:  "net1",
			GuestMAC: guestMAC,
			HostIF:   args.Net.TapDev,
		})
	}

	drives := make([]FirecrackerDrive, 0)
	for _, blockArg := range ukernel.MonitorBlockCli() {
		d := FirecrackerDrive{
			DriveID:   blockArg.ID,
			IsRO:      false,
			IsRootDev: false,
			HostPath:  blockArg.Path,
		}
		if blockArg.ID == "rootfs" {
			d.IsRootDev = true
		}
		drives = append(drives, d)
	}

	source := FirecrackerBootSource{
		ImagePath:  args.UnikernelPath,
		BootArgs:   args.Command,
		InitrdPath: initrdPath,
	}

	var vsock FirecrackerVSockDev
	if args.VAccelType == "vsock" {
		vsock = FirecrackerVSockDev{
			GuestCID: args.VSockDevID,
			UDSPath:  args.VSockDevPath + "/vaccel.sock",
			VSockID:  "root",
		}
	}

	return &FirecrackerConfig{
		Source:  source,
		Machine: machine,
		Drives:  drives,
		NetIfs:  nets,
		VSock:   vsock,
	}
}

// PreExec performs pre-execution setup. Firecracker has no special pre-exec requirements.
func (fc *Firecracker) PreExec(_ types.ExecArgs) error {
	return nil
}

