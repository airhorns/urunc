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
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest is what the fake server saw on a single connection.
type recordedRequest struct {
	Method      string
	Path        string
	Body        string
	ContentType string
}

// fakeFCServer implements just enough of firecracker's HTTP-over-UDS API to
// test our client: accept one request per connection, record it, and reply
// with either a configurable status+body.
type fakeFCServer struct {
	sock string
	ln   net.Listener

	mu    sync.Mutex
	reqs  []recordedRequest
	reply func(req recordedRequest) (status int, body string)

	done chan struct{}
}

func startFakeFCServer(t *testing.T) *fakeFCServer {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeFCServer{
		sock: sock,
		ln:   ln,
		reply: func(_ recordedRequest) (int, string) {
			return 204, ""
		},
		done: make(chan struct{}),
	}
	go s.accept(t)
	return s
}

func (s *fakeFCServer) close() {
	_ = s.ln.Close()
	<-s.done
}

func (s *fakeFCServer) setReply(f func(recordedRequest) (int, string)) {
	s.mu.Lock()
	s.reply = f
	s.mu.Unlock()
}

func (s *fakeFCServer) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedRequest, len(s.reqs))
	copy(out, s.reqs)
	return out
}

func (s *fakeFCServer) accept(t *testing.T) {
	defer close(s.done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(t, conn)
	}
}

func (s *fakeFCServer) handle(t *testing.T, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	var body []byte
	if req.ContentLength > 0 {
		body = make([]byte, req.ContentLength)
		if _, err := io.ReadFull(br, body); err != nil {
			t.Logf("read body: %v", err)
			return
		}
	}
	rec := recordedRequest{
		Method:      req.Method,
		Path:        req.URL.Path,
		Body:        string(body),
		ContentType: req.Header.Get("Content-Type"),
	}

	s.mu.Lock()
	s.reqs = append(s.reqs, rec)
	reply := s.reply
	s.mu.Unlock()

	status, respBody := reply(rec)

	resp := "HTTP/1.1 " + http.StatusText(status) + "\r\n"
	// Status code line must be "HTTP/1.1 <code> <text>\r\n".
	resp = "HTTP/1.1 "
	resp += itoa(status) + " " + http.StatusText(status) + "\r\n"
	resp += "Content-Type: application/json\r\n"
	resp += "Content-Length: " + itoa(len(respBody)) + "\r\n"
	resp += "Connection: close\r\n\r\n"
	resp += respBody
	_, _ = conn.Write([]byte(resp))
}

func itoa(n int) string {
	// avoid strconv import just for this
	if n == 0 {
		return "0"
	}
	var digits []byte
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

func TestFCAPIClientPutSuccess(t *testing.T) {
	s := startFakeFCServer(t)
	defer s.close()

	c := newFCAPIClient(s.sock)
	if err := c.Put("/machine-config", map[string]any{"vcpu_count": 1, "mem_size_mib": 128}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("want 1 request, got %d", len(got))
	}
	if got[0].Method != "PUT" || got[0].Path != "/machine-config" {
		t.Fatalf("bad request line: %+v", got[0])
	}
	if got[0].ContentType != "application/json" {
		t.Fatalf("bad content type: %q", got[0].ContentType)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got[0].Body), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["vcpu_count"].(float64) != 1 {
		t.Fatalf("bad body: %v", body)
	}
}

func TestFCAPIClientPatchSuccess(t *testing.T) {
	s := startFakeFCServer(t)
	defer s.close()

	c := newFCAPIClient(s.sock)
	if err := c.Patch("/vm", map[string]string{"state": "Paused"}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got := s.requests()
	if len(got) != 1 || got[0].Method != "PATCH" || got[0].Path != "/vm" {
		t.Fatalf("bad request: %+v", got)
	}
}

func TestFCAPIClient204NoContent(t *testing.T) {
	s := startFakeFCServer(t)
	defer s.close()
	s.setReply(func(recordedRequest) (int, string) { return 204, "" })

	c := newFCAPIClient(s.sock)
	if err := c.Put("/actions", map[string]string{"action_type": "InstanceStart"}); err != nil {
		t.Fatalf("204 should be success: %v", err)
	}
}

func TestFCAPIClientErrorStatus(t *testing.T) {
	s := startFakeFCServer(t)
	defer s.close()
	s.setReply(func(recordedRequest) (int, string) {
		return 400, `{"fault_message":"bad"}`
	})

	c := newFCAPIClient(s.sock)
	err := c.Put("/boot-source", map[string]string{"kernel_image_path": "/nope"})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("error should mention HTTP 400, got: %v", err)
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Fatalf("error should include fault body, got: %v", err)
	}
}

func TestFCAPIClientWaitForSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "late.sock")

	c := newFCAPIClient(sock)

	ready := make(chan error, 1)
	go func() {
		ready <- c.waitForSocket(1 * time.Second)
	}()

	// simulate fc creating the socket 150ms later
	time.Sleep(150 * time.Millisecond)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("waitForSocket: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("waitForSocket blocked past deadline")
	}
}

func TestFCAPIClientWaitForSocketTimeout(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "never.sock")
	_ = os.Remove(sock)

	c := newFCAPIClient(sock)
	start := time.Now()
	err := c.waitForSocket(200 * time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("returned too early: %v", elapsed)
	}
}
