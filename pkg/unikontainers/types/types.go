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

//revive:disable:var-naming
package types

type Unikernel interface {
	Init(UnikernelParams) error
	CommandString() (string, error)
	SupportsBlock() bool
	SupportsFS(string) bool
	MonitorNetCli(string, string) string
	MonitorBlockCli() []MonitorBlockArgs
	MonitorCli() MonitorCliArgs
}

type VMM interface {
	// BuildExecCmd builds and validates the VMM command arguments without executing.
	// This is used to verify the command can be built before reporting container as started.
	// The returned slice contains the command path as the first element followed by arguments.
	BuildExecCmd(args ExecArgs, ukernel Unikernel) ([]string, error)
	// PreExec performs any monitor-specific setup that must happen after BuildExecCmd
	// succeeds but before syscall.Exec is called. For example, HVT applies seccomp
	// filters here. Most monitors can return nil (no-op).
	PreExec(args ExecArgs) error
	Stop(int) error
	Path() string
	UsesKVM() bool
	SupportsSharedfs(string) bool
	Ok() error
}

// SnapshotVMM is implemented by monitors that can run in supervised,
// API-driven mode to produce or restore a template snapshot rather than
// cold-booting via syscall.Exec + --config-file. Only firecracker implements
// this today.
type SnapshotVMM interface {
	VMM
	// Supervise spawns the VMM with an API socket and either creates a
	// template snapshot (when args.Snapshot.CacheHit is false) or restores
	// one (when true), then blocks until the child exits. It encapsulates
	// everything BuildExecCmd + syscall.Exec would have done for this
	// container.
	Supervise(args ExecArgs, ukernel Unikernel) error
}

// SnapshotArgs carries the resolved paths and parameters required for a
// supervised firecracker invocation. Nil on ExecArgs means cold-boot.
type SnapshotArgs struct {
	// CacheDir is the absolute path to the cache entry directory
	// (/var/lib/urunc/snapshots/<key>) as visible from the monitor rootfs
	// after pivot.
	CacheDir string
	// VmstatePath / MemPath are absolute paths to the snapshot files. On
	// a cache hit, they point at the committed files; on a miss they point
	// at the .tmp files used during build.
	VmstatePath string
	MemPath     string
	// CacheHit is true when a valid template exists; false means this run
	// must build the template as part of its first boot.
	CacheHit bool
	// GuestMAC is the MAC address that will be used for the guest
	// network interface. On build it is the deterministic template MAC;
	// on restore it is read from the committed manifest.
	GuestMAC string
	// IfaceID is the firecracker network interface identifier used both
	// at build time (for the initial PUT /network-interfaces) and at
	// restore time (for network_overrides).
	IfaceID string
	// ReadyDelayMs is how long to sleep between InstanceStart and
	// /snapshot/create on the build path. Ignored on restore.
	ReadyDelayMs uint
	// APISocketPath is the Unix-domain socket path firecracker should
	// listen on. Must be writable in both the host and the pivoted
	// monitor rootfs.
	APISocketPath string
	// CommitFn is called on a successful build after /snapshot/create
	// completes and the VM has been resumed. It is expected to rename
	// the .tmp files into their committed names and write manifest.json.
	// Nil on the restore path.
	CommitFn func() error
}

type NetDevParams struct {
	IP      string // The veth device IP
	Mask    string // The veth device mask
	Gateway string // The veth device gateway
	MAC     string // The MAC address of the guest network device
	TapDev  string // The tap device name
	MTU     int    // The MTU value of the tap device
}

type BlockDevParams struct {
	Source     string
	MountPoint string
	FsType     string
	ID         string
}

type SharedfsParams struct {
	Type string // The type of shared-fs 9p or virtiofs
	Path string // The path in the host to share with guest
}

type RootfsParams struct {
	Type        string // The type of rootfs (block, initrd, 9pfs, virtiofs)
	Path        string // The path in the host where rootfs resides
	MountedPath string // The mountpoint in the host where the rootfs is mounted
	MonRootfs   string // The rootfs for the monitor process
}

// Specific to Linux
type ProcessConfig struct {
	UID     uint32 // The uid of the process inside the guest
	GID     uint32 // The gid of the process inside the guest
	WorkDir string // The workdir of the process inside the guest
}

// UnikernelParams holds the data required to build the unikernels commandline
type UnikernelParams struct {
	CmdLine    []string // The cmdline provided by the image
	EnvVars    []string // The environment variables provided by the image
	Monitor    string   // The monitor where guest will execute
	Version    string   // The version of the unikernel
	InitrdPath string   // The path to the initrd of the unikernel
	Net        NetDevParams
	Block      []BlockDevParams
	Rootfs     RootfsParams  // Information about rootfs
	ProcConf   ProcessConfig // Information for the process execution inside the guest
}

// ExecArgs holds the data required by Execve to start the VMM
// FIXME: add extra fields if required by additional VMM's
type ExecArgs struct {
	ContainerID   string   // The container ID
	Environment   []string // The environment variables of the monitor
	Command       string   // The unikernel's command line
	Seccomp       bool     // Enable or disable seccomp filters for the VMM
	MemSizeB      uint64   // The size of the memory provided to the VM in bytes
	VCPUs         uint     // The number of vCPUs to allocate
	UnikernelPath string   // The path of the unikernel inside rootfs
	InitrdPath    string   // The path to the initrd of the unikernel
	VAccelType    string   // Specifies the vAccel acceleration type(e.g. vsock). When empty, vAccel is disabled
	VSockDevPath  string   // The host directory where the fc unix socket is created
	VSockDevID    int      // The guest-cid
	Net           NetDevParams
	Sharedfs      SharedfsParams
	// Snapshot is nil for cold-boot containers. When non-nil, the VMM must
	// implement SnapshotVMM and urunc drives a supervised API lifecycle
	// instead of syscall.Exec. See SnapshotArgs.
	Snapshot *SnapshotArgs
}

type MonitorCliArgs struct {
	ExtraInitrd string
	OtherArgs   string
}

type MonitorBlockArgs struct {
	ID        string
	Path      string
	ExactArgs string
}

// ExtraBinConfig struct is used to hold specific configuration for extra binaries
// like virtiofsd. It is parsed from the urunc config file or state.json annotations
type ExtraBinConfig struct {
	Path    string `toml:"path"`              // The path to the binary
	Options string `toml:"options,omitempty"` // Optional cli options for the extra binary
}

// MonitorConfig struct is used to hold hypervisor specific configuration
// that is parsed from the urunc config file or state.json annotations
type MonitorConfig struct {
	DefaultMemoryMB uint   `toml:"default_memory_mb"`
	DefaultVCPUs    uint   `toml:"default_vcpus"`
	BinaryPath      string `toml:"path,omitempty"`      // Optional path to the hypervisor binary
	DataPath        string `toml:"data_path,omitempty"` // Optional path to the hypervisor data files (e.g. qemu bios stuff)
	Vhost           bool   `toml:"vhost,omitempty"`     // Optional: enable vhost for network performance optimization
}
