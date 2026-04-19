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
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// fcAPIDialTimeout is used to connect to the UDS; short because the socket
// is local and already known to exist. fcAPIRequestTimeout is the ceiling on
// any one request; /snapshot/create with multi-GiB memory can take several
// seconds because fc fsyncs the mem file before returning.
const (
	fcAPIDialTimeout    = 5 * time.Second
	fcAPIRequestTimeout = 120 * time.Second
)

// fcAPIClient is a minimal HTTP-over-Unix-domain client for firecracker's
// management socket. We don't use net/http.Client here because the wire
// semantics are simple and we want zero dependencies beyond stdlib. Pattern
// mirrors zeroboot's src/vmm/firecracker.rs:151-183.
type fcAPIClient struct {
	socketPath string
}

func newFCAPIClient(socketPath string) *fcAPIClient {
	return &fcAPIClient{socketPath: socketPath}
}

// waitForSocket blocks until the firecracker API socket file appears, up to
// timeout. Firecracker creates the socket shortly after startup.
func (c *fcAPIClient) waitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(c.socketPath); err == nil {
			// Give fc a moment to actually bind() before we connect.
			time.Sleep(20 * time.Millisecond)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker API socket %s did not appear within %s", c.socketPath, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *fcAPIClient) Put(path string, body any) error {
	return c.request("PUT", path, body)
}

func (c *fcAPIClient) Patch(path string, body any) error {
	return c.request("PATCH", path, body)
}

func (c *fcAPIClient) request(method, path string, body any) error {
	conn, err := net.DialTimeout("unix", c.socketPath, fcAPIDialTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(fcAPIRequestTimeout))

	var buf []byte
	if body != nil {
		buf, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s body: %w", method, path, err)
		}
	}

	req := &bytes.Buffer{}
	fmt.Fprintf(req, "%s %s HTTP/1.1\r\n", method, path)
	fmt.Fprintf(req, "Host: localhost\r\n")
	fmt.Fprintf(req, "Accept: application/json\r\n")
	fmt.Fprintf(req, "Content-Type: application/json\r\n")
	fmt.Fprintf(req, "Content-Length: %d\r\n", len(buf))
	fmt.Fprintf(req, "Connection: close\r\n\r\n")
	req.Write(buf)

	if _, err := conn.Write(req.Bytes()); err != nil {
		return fmt.Errorf("write %s %s: %w", method, path, err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// Firecracker uses 204 (No Content) for most successful configuration
	// calls and 200 for a small number (/snapshot/create returns 204 too).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBody))
	}
	return nil
}
