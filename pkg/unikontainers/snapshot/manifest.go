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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Manifest is the sidecar metadata describing a cached template snapshot.
// The snapshot/memory files themselves live next to it in the cache dir.
type Manifest struct {
	Version      int       `json:"version"`
	CacheKey     string    `json:"cache_key"`
	GuestMAC     string    `json:"guest_mac"`
	IfaceID      string    `json:"iface_id"`
	MemMiB       uint64    `json:"mem_mib"`
	VCPUs        uint      `json:"vcpus"`
	FCVersion    string    `json:"fc_version"`
	Arch         string    `json:"arch"`
	CreatedAt    time.Time `json:"created_at"`
	UnikernelBin string    `json:"unikernel_bin_sha256"`
}

const (
	ManifestFilename = "manifest.json"
	VmstateFilename  = "vmstate"
	MemFilename      = "mem"
	LockFilename     = ".lock"

	currentManifestVersion = 1
)

func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFilename))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// WriteManifest serializes m and writes it atomically via tmp + rename.
func WriteManifest(dir string, m *Manifest) error {
	if m.Version == 0 {
		m.Version = currentManifestVersion
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ManifestFilename+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, ManifestFilename))
}
