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

//go:build !linux

package hypervisors

import (
	"errors"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// Supervise is a non-Linux stub — firecracker itself only runs on Linux, so
// this code path is only usable there. Keeps the hypervisors package
// buildable on darwin for local development / tests.
func (fc *Firecracker) Supervise(_ types.ExecArgs, _ types.Unikernel) error {
	return errors.New("firecracker supervised snapshot path is only supported on linux")
}
