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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
)

// KeyInputs are the fields hashed into a cache key. Two template instances
// collide if and only if all fields are equal.
//
// The caller is responsible for resolving any environment-derived values
// (firecracker version, arch) before passing them in.
type KeyInputs struct {
	UnikernelBinaryPath string
	InitrdPath          string
	UnikernelType       string
	UnikernelVersion    string
	Hypervisor          string
	FCBinaryPath        string
	FCVersion           string
	Arch                string
	MemMiB              uint64
	VCPUs               uint
	GuestCmdLine        string
	KernelBootArgs      string
	ExtraBlocks         []string
}

// ComputeKey hashes the inputs into a hex-encoded sha256. File-backed inputs
// (UnikernelBinaryPath, InitrdPath) are read and hashed; other fields are
// serialized as key=value pairs in sorted order.
//
// The unikernel binary's content sha is also returned so the caller can
// persist it in the manifest for diagnostics.
func ComputeKey(in KeyInputs) (cacheKey, unikernelSha string, err error) {
	unikernelSha, err = hashFile(in.UnikernelBinaryPath)
	if err != nil {
		return "", "", fmt.Errorf("hash unikernel binary: %w", err)
	}
	initrdSha := ""
	if in.InitrdPath != "" {
		initrdSha, err = hashFile(in.InitrdPath)
		if err != nil {
			return "", "", fmt.Errorf("hash initrd: %w", err)
		}
	}

	fields := map[string]string{
		"unikernel_sha":     unikernelSha,
		"initrd_sha":        initrdSha,
		"unikernel_type":    in.UnikernelType,
		"unikernel_version": in.UnikernelVersion,
		"hypervisor":        in.Hypervisor,
		"fc_binary":         in.FCBinaryPath,
		"fc_version":        in.FCVersion,
		"arch":              in.Arch,
		"mem_mib":           fmt.Sprintf("%d", in.MemMiB),
		"vcpus":             fmt.Sprintf("%d", in.VCPUs),
		"cmdline":           in.GuestCmdLine,
		"boot_args":         in.KernelBootArgs,
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, fields[k])
	}
	blocks := append([]string(nil), in.ExtraBlocks...)
	sort.Strings(blocks)
	for _, b := range blocks {
		fmt.Fprintf(h, "block=%s\n", b)
	}

	return hex.EncodeToString(h.Sum(nil)), unikernelSha, nil
}

func hashFile(path string) (string, error) {
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
