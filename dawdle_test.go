// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package dawdle

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

type testTcpServer struct {
	ln      net.Listener
	reader  *io.PipeReader
	writer  *io.PipeWriter
	buf     *bytes.Buffer
	bufLock *sync.RWMutex
}

func runTestTcpServer(t *testing.T) *testTcpServer {
	t.Helper()

	s := &testTcpServer{
		buf:     new(bytes.Buffer),
		bufLock: new(sync.RWMutex),
	}
	s.reader, s.writer = io.Pipe()
	var err error
	s.ln, err = net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			conn, err := s.ln.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					panic(err)
				}

				return
			}

			go func(c net.Conn) {
				io.Copy(s.writer, c)
				c.Close()
			}(conn)

			go func() {
				buf := make([]byte, 1)
				var err error
				for {
					_, err = s.reader.Read(buf)
					if err != nil {
						if err == io.EOF {
							return
						}
						panic(err)
					}
					s.bufLock.Lock()
					_, err := s.buf.Write(buf)
					s.bufLock.Unlock()
					if err != nil {
						panic(err)
					}
				}
			}()
		}
	}()

	return s
}

func (s *testTcpServer) Close() {
	s.ln.Close()
}

func (s *testTcpServer) Addr() string {
	return s.ln.Addr().String()
}

func (s *testTcpServer) BufLen() int {
	s.bufLock.RLock()
	defer s.bufLock.RUnlock()
	return s.buf.Len()
}

func (s *testTcpServer) BufBytes() []byte {
	s.bufLock.RLock()
	defer s.bufLock.RUnlock()
	return s.buf.Bytes()
}

func (s *testTcpServer) WaitBuffer(expected []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	doneCh := make(chan struct{})

	go func() {
		for {
			if ctx.Err() != nil {
				return
			}

			if bytes.Equal(s.BufBytes(), expected) {
				close(doneCh)
				return
			}

			// Sleep 10ms to keep things relatively sane
			time.Sleep(time.Millisecond * 10)
		}
	}()

	var err error
	select {
	case <-doneCh:
	case <-ctx.Done():
		close(doneCh)
		err = ctx.Err()
	}

	if err != nil {
		return fmt.Errorf("error waiting for buffer, expectedlen=%d,actuallen=%d, err: %s", len(expected), s.BufLen(), err)
	}

	return nil
}

func TestTestTcpServer(t *testing.T) {
	ts := runTestTcpServer(t)
	if ts.BufLen() != 0 {
		t.Fatal("test buffer should be zero")
	}

	defer ts.Close()

	// Test sending some data and check buffer
	conn, err := net.Dial("tcp", ts.Addr())
	if err != nil {
		t.Fatal(err)
	}

	expected := "foobar"
	if _, err := conn.Write([]byte(expected)); err != nil {
		t.Fatal(err)
	}

	conn.Close()
	// Wait for a non-zero buffer length.
	if err := ts.WaitBuffer([]byte(expected)); err != nil {
		t.Fatal(err)
	}

	if string(ts.BufBytes()) != expected {
		t.Fatalf("expected buffer to have %v, got %v", expected, string(ts.BufBytes()))
	}
}

func TestProxy(t *testing.T) {
	ts := runTestTcpServer(t)
	if ts.BufLen() != 0 {
		t.Fatal("test buffer should be zero")
	}

	defer ts.Close()

	// Create the proxy
	proxy, err := NewProxy("tcp", ":0", ts.Addr())
	if err != nil {
		t.Fatal(err)
	}

	defer proxy.Close()
	proxy.Start()

	// Connect to the proxy. Disable the connection's write buffer so
	// that it's not interfering with our test father down (we can
	// always expect the proxy buffer to be the only buffer we need to
	// care about).
	conn, err := net.Dial("tcp", proxy.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).SetWriteBuffer(1); err != nil {
		t.Fatal(err)
	}

	// Start writing bytes (not string), and checking bytes here. We
	// want a size that is going to exhaust the proxy buffer, so we
	// create a buffer of default size * 2.

	writeBuffer := make([]byte, defaultBufferSize*2) // 65536 bytes (32768 * 2)
	var expectedB []byte

	// ***********************
	// ** Normal write test **
	// ***********************

	// Perform this test twice to do a standard test of the proxy.
	for i := 1; i < 3; i++ {
		actualN, err := rand.Read(writeBuffer)
		if err != nil {
			t.Fatalf("basic write %d: err: %s", i, err)
		}
		if actualN != len(writeBuffer) {
			t.Fatalf("basic write %d: expected to read %d bytes, got %d", i, len(writeBuffer), actualN)
		}

		expectedB = append(expectedB, writeBuffer...)

		actualN, err = conn.Write(writeBuffer)
		if err != nil {
			t.Fatalf("basic write %d: err: %s", i, err)
		}
		if actualN != len(writeBuffer) {
			t.Fatalf("basic write %d: expected to write %d bytes, got %d", i, len(writeBuffer), actualN)
		}

		if err := ts.WaitBuffer(expectedB); err != nil {
			t.Fatalf("basic write %d: err: %s", i, err)
		}
	}

	// since we performed the test twice i.e. sent writeBuffer twice, we have actually sent 131072 (65536 * 2) bytes until now

	// ***********
	// ** Pause **
	// ***********

	// Pause the connection, and set a deadline. We expect this to
	// fail, with the write maxing out the buffer before hanging.
	proxy.Pause()
	conn.SetWriteDeadline(time.Now().Add(time.Second))

	actualN, err := rand.Read(writeBuffer)
	if err != nil {
		t.Fatal(err)
	}
	if actualN != len(writeBuffer) {
		t.Fatalf("expected to read %d bytes, got %d", len(writeBuffer), actualN)
	}

	// on below conn.Write, any number of bytes may have been sent to other side depending on
	// 1. how quickly the test executes (on Mac we have seen it actually 1 second to reach here, on Github runners it can reach within micro-seconds also)
	// 2. the actual TCP buffer size set by the OS depending upon it's minimum limit even if we set it actually to 1 byte at line 194
	// the error may or may not occur depending upon how quickly the test ran on a system. But if the error does occur, we want to make sure
	// it is a DeadlineExceeded error due to the Write deadline triggering.
	actualN, err = conn.Write(writeBuffer)
	if err != nil {
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			// Unexpected error
			t.Fatal(err)
		}
	}
	// Even though we've set the write buffer on the socket to 1, this
	// still does not seem to be enough to get a full even write, so we
	// are going to have to determine the remainder of what we did not
	// write to see what else we need to write to get the complete
	// picture when we resume.
	//
	// First test to see that we at least wrote out our proxy buffer

	// Different OS have underlying TCP settings that can cause a varying number of bytes to be sent in before the WriteDeadline kicks in
	// this doesn't just depend on the internal TCP write buffer size (which we reduced to 1 byte at line 194), but can be dependent on various other factors
	// so we will just log here the actual number of bytes we were able to send
	fmt.Printf("we actually sent %d number of bytes before the write deadline kicked in, ", actualN)

	// Save bytes remaining to be sent, so we copy all contents of writeBuffer from actualN (the ones we sent to rest of it), in some cases if
	// we sent all the data already, then this remainder can be empty too.
	remainder := writeBuffer[actualN:]

	// Incase some bytes were actually sent, now we want to verify that those many bytes have been seen by the receiver. If the complete buffer was sent here, then
	// the expectedB would be in a total of 196608 bytes (131072 sent earlier + 65536 the full writeBuffer length sent now)
	expectedB = append(expectedB, writeBuffer[:actualN+1]...)
	if err := ts.WaitBuffer(expectedB); err != nil {
		t.Fatal(err)
	}

	// ************
	// ** Resume **
	// ************

	// Resume the connection. First we do a write out of our remaining
	// bytes, if any, and test that everything made it (including any
	// still in the OS buffer). Then we perform a final regular write
	// to ensure everything is functional.
	conn.SetWriteDeadline(time.Time{})
	proxy.Resume()

	if len(remainder) > 0 {
		actualN, err = conn.Write(remainder)
		if err != nil {
			t.Fatal(err)
		}

		if actualN != len(remainder) {
			t.Fatalf("expected to write %d bytes, got %d", len(remainder), actualN)
		}
	}

	// Expect the rest of the "remainder" bytes are now sent to the other side, incase before resuming we sent only some contents.
	// at this point we should definitely have "expectedB" to be a totla of 196608 because even if we sent half contents between Pause and Resume, the rest
	// have been sent after resume so this should now be exactly that total.
	expectedB = append(expectedB, remainder...)
	if err := ts.WaitBuffer(expectedB); err != nil {
		t.Fatal(err)
	}

	// Final write starts here to ensure our proxy is fully functional now
	actualN, err = rand.Read(writeBuffer)
	if err != nil {
		t.Fatal(err)
	}
	if actualN != len(writeBuffer) {
		t.Fatalf("expected to read %d bytes, got %d", len(writeBuffer), actualN)
	}

	actualN, err = conn.Write(writeBuffer)
	if err != nil {
		t.Fatal(err)
	}
	if actualN != len(writeBuffer) {
		t.Fatalf("expected to write %d bytes, got %d", len(writeBuffer), actualN)
	}
	expectedB = append(expectedB, writeBuffer...)
	if err := ts.WaitBuffer(expectedB); err != nil {
		t.Fatal(err)
	}
}

func TestNewProxyBadTCPAddress(t *testing.T) {
	_, err := NewProxy("tcp", "", "bad+addr")
	if err == nil {
		t.Fatal("expected error, got none")
	}

	if err.Error() != "error creating proxy: address bad+addr: missing port in address" {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestNewProxyBadProto(t *testing.T) {
	_, err := NewProxy("bad", "", "")
	if err == nil {
		t.Fatal("expected error, got none")
	}

	if err.Error() != "error creating proxy: unsupported protocol bad" {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestNewProxyBuffers(t *testing.T) {
	ts := runTestTcpServer(t)
	if ts.BufLen() != 0 {
		t.Fatal("test buffer should be zero")
	}

	defer ts.Close()

	// Create the proxy
	proxy, err := NewProxy("tcp", ":0", ts.Addr(), WithRbufSize(1), WithWbufSize(2))
	if err != nil {
		t.Fatal(err)
	}

	defer proxy.Close()

	if proxy.rbufSize != 1 {
		t.Fatalf("expected rbufSize=1, got %d", proxy.rbufSize)
	}
	if proxy.wbufSize != 2 {
		t.Fatalf("expected wbufSize=2, got %d", proxy.wbufSize)
	}
}

func testWithBadOpt() func(p *proxy) error {
	return func(p *proxy) error {
		return errors.New("foobar")
	}
}

func TestNewProxyBadOpt(t *testing.T) {
	_, err := NewProxy("tcp", "", "localhost:12345", testWithBadOpt())
	if err == nil {
		t.Fatal("expected error, got none")
	}

	if err.Error() != "error creating proxy: foobar" {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestNewProxyListenErr(t *testing.T) {
	proxy, err := NewProxy("tcp", "bad+addr", "localhost:12345")
	if err != nil {
		t.Fatal(err)
	}

	defer proxy.Close()

	err = proxy.Start()
	if err == nil {
		t.Fatal("expected error, got none")
	}

	if err.Error() != "error starting listener: listen tcp: address bad+addr: missing port in address" {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestNewProxyWithListener(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}

	proxy, err := NewProxy("tcp", "bad+addr", "localhost:12345", WithListener(ln))
	if err != nil {
		t.Fatal(err)
	}

	defer proxy.Close()

	err = proxy.Start()
	if err == nil {
		t.Fatal("expected error, got none")
	}

	if err.Error() != "error starting listener: listener already started" {
		t.Fatalf("unexpected error: %s", err)
	}

	if proxy.ln != ln {
		t.Fatal("unexpected listener set")
	}
}

// TODO: Fix this so that it works. Timing/buffering issues make this
// frustratingly impossible.
//
// func TestProxyRemoteConnectErr(t *testing.T) {
// 	proxy, err := NewProxy("tcp", ":0", "localhost:0", WithLogger(log.Default()))
// 	if err != nil {
// 		t.Fatal(err)
// 	}
// 	defer proxy.Close()
//
// 	if err := proxy.Start(); err != nil {
// 		t.Fatal(err)
// 	}
//
// 	conn, err := net.Dial("tcp", proxy.ListenerAddr())
// 	if err != nil {
// 		t.Fatal(err)
// 	}
// 	defer conn.Close()
//
// 	n, err := conn.Write([]byte("foobar"))
// 	if err == nil {
// 		t.Fatal("expected error, got none")
// 	}
//
// 	// Connection should have closed right away
// 	if n != 0 {
// 		t.Fatalf("expected 0 bytes written")
// 	}
// }

func TestProxyConnMap(t *testing.T) {
	ts := runTestTcpServer(t)
	if ts.BufLen() != 0 {
		t.Fatal("test buffer should be zero")
	}

	defer ts.Close()

	// Create the proxy
	proxy, err := NewProxy("tcp", ":0", ts.Addr())
	if err != nil {
		t.Fatal(err)
	}

	defer proxy.Close()
	proxy.Start()

	// Create two connections
	c1, err := net.Dial("tcp", proxy.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()

	// Create two connections
	c2, err := net.Dial("tcp", proxy.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	// Expect entires in the map
	c1addr := c1.LocalAddr().String()
	c2addr := c2.LocalAddr().String()
	proxy.conns.RLock()
	if lenM := len(proxy.conns.m); lenM != 2 {
		proxy.conns.RUnlock()
		t.Fatalf("expected connection map to have 2 entries, got %d", lenM)
	}
	if _, ok := proxy.conns.m[c1addr]; !ok {
		proxy.conns.RUnlock()
		t.Fatalf("connection key %s not in map", c1addr)
	}
	if _, ok := proxy.conns.m[c2addr]; !ok {
		proxy.conns.RUnlock()
		t.Fatalf("connection key %s not in map", c2addr)
	}
	proxy.conns.RUnlock()

	// Close a connection
	c1.Close()
	if err := waitConnMapLen(proxy.conns, 1); err != nil {
		t.Fatal(err)
	}
	proxy.conns.RLock()
	if _, ok := proxy.conns.m[c1addr]; ok {
		proxy.conns.RUnlock()
		t.Fatalf("connection key %s should not be in map", c1addr)
	}
	proxy.conns.RUnlock()

	// Close the other
	c2.Close()
	if err := waitConnMapLen(proxy.conns, 0); err != nil {
		t.Fatal(err)
	}
	proxy.conns.RLock()
	if _, ok := proxy.conns.m[c2addr]; ok {
		proxy.conns.RUnlock()
		t.Fatalf("connection key %s should not be in map", c1addr)
	}
	proxy.conns.RUnlock()
}

func waitConnMapLen(cm *connMap, expected int) error {
	var lenM int
	for i := 0; i < 100; i++ {
		cm.RLock()
		lenM = len(cm.m)
		cm.RUnlock()
		if lenM == expected {
			return nil
		}

		time.Sleep(time.Millisecond * 10)
	}

	return fmt.Errorf("timeout waiting for map to be len=%d, got %d", expected, lenM)
}

func TestProxyCloseConnections(t *testing.T) {
	ts := runTestTcpServer(t)
	if ts.BufLen() != 0 {
		t.Fatal("test buffer should be zero")
	}

	defer ts.Close()

	// Create the proxy
	proxy, err := NewProxy("tcp", ":0", ts.Addr())
	if err != nil {
		t.Fatal(err)
	}

	defer proxy.Close()
	proxy.Start()

	// Create two connections
	c1, err := net.Dial("tcp", proxy.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()

	// Create two connections
	c2, err := net.Dial("tcp", proxy.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	// Expect entires in the map
	c1addr := c1.LocalAddr().String()
	c2addr := c2.LocalAddr().String()
	if err := waitConnMapLen(proxy.conns, 2); err != nil {
		t.Fatal(err)
	}
	if _, ok := proxy.conns.m[c1addr]; !ok {
		t.Fatalf("connection key %s not in map", c1addr)
	}
	if _, ok := proxy.conns.m[c2addr]; !ok {
		t.Fatalf("connection key %s not in map", c2addr)
	}

	// Start reads in goroutines, these should block until we kill them
	var wg sync.WaitGroup
	wg.Add(2)
	var b1, b2 []byte
	go func() {
		b1, _ = io.ReadAll(c1)
		wg.Done()
	}()
	go func() {
		b2, _ = io.ReadAll(c2)
		wg.Done()
	}()
	// Kill all connections and wait
	if err := proxy.CloseConnections(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	// Assert 0 bytes read
	if len(b1) != 0 {
		t.Error("should have read 0 bytes from c1")
	}
	if len(b2) != 0 {
		t.Error("should have read 0 bytes from c2")
	}
	// Assert errors. TODO: Fix this, or remove it... apparently this is giving
	// nil, not too sure why.
	//
	// if !errors.Is(err1, net.ErrClosed) {
	// 	t.Errorf("expected err1 to be net.ErrClosed, got %s", err)
	// }
	// if !errors.Is(err2, net.ErrClosed) {
	// 	t.Errorf("expected err2 to be net.ErrClosed, got %s", err)
	// }
	// Assert we have an empty map
	if err := waitConnMapLen(proxy.conns, 0); err != nil {
		t.Fatal(err)
	}

	// Finally assert that killing current connections did not break ability to
	// create a new connection.
	c3, err := net.Dial("tcp", proxy.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()

	if _, err := io.WriteString(c3, "foobar"); err != nil {
		t.Fatal(err)
	}

	c3.Close()
	if err := ts.WaitBuffer([]byte("foobar")); err != nil {
		t.Fatal(err)
	}
}

func TestProxyPauseNewConn(t *testing.T) {
	ts := runTestTcpServer(t)
	if ts.BufLen() != 0 {
		t.Fatal("test buffer should be zero")
	}

	defer ts.Close()

	// Create the proxy
	proxy, err := NewProxy("tcp", ":0", ts.Addr())
	if err != nil {
		t.Fatal(err)
	}

	defer proxy.Close()
	proxy.Start()

	// Pause immediately
	proxy.Pause()

	// Connect
	conn, err := net.Dial("tcp", proxy.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// This should succeed, due to buffering on the OS side (we can fix it if the
	// test ever fails)
	if _, err := io.WriteString(conn, "foobar"); err != nil {
		t.Fatal(err)
	}

	// ... but should not have made it to the buffer
	if err := ts.WaitBuffer([]byte("foobar")); err == nil {
		t.Fatal("expected no update to test server buffer")
	}

	if ts.BufLen() != 0 {
		t.Fatal("expected zero buffer size")
	}

	// Unpause, this should send the data
	proxy.Resume()
	if err := ts.WaitBuffer([]byte("foobar")); err != nil {
		t.Fatal(err)
	}
}
