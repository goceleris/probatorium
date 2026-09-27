package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestSSH_SignalIsBounded: a signal over a link that has stopped answering
// must give up, not block. A seed's teardown sends SIGTERM synchronously and
// only then arms its SIGKILL timer, so an unbounded signal stalled the whole
// seed loop for as long as the host was gone (probatorium#415). The server
// here accepts the session and never answers its exec request, which is what
// a wedged host looks like from the client.
func TestSSH_SignalIsBounded(t *testing.T) {
	hostKey, err := func() (ssh.Signer, error) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		return ssh.NewSignerFromKey(priv)
	}()
	if err != nil {
		t.Fatal(err)
	}
	srvCfg := &ssh.ServerConfig{NoClientAuth: true}
	srvCfg.AddHostKey(hostKey)

	// A loopback listener, not net.Pipe: both ends of an SSH handshake write
	// their version line before reading, which deadlocks an unbuffered pipe.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	execSeen := make(chan struct{}, 1)
	srvConns := make(chan net.Conn, 1)
	t.Cleanup(func() {
		select {
		case c := <-srvConns:
			_ = c.Close()
		default:
		}
	})
	go func() {
		srvConn, err := ln.Accept()
		if err != nil {
			return
		}
		srvConns <- srvConn
		_, chans, reqs, err := ssh.NewServerConn(srvConn, srvCfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			ch, chReqs, err := nc.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer func() { _ = ch.Close() }()
				for req := range chReqs {
					if req.Type == "exec" {
						select {
						case execSeen <- struct{}{}:
						default:
						}
					}
					// Never reply: the client's exec waits for an answer
					// that does not come.
				}
			}()
		}
	}()
	cliConn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cliConn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(cliConn, ln.Addr().String(), &ssh.ClientConfig{
		User:            "probatorium",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // in-process test server
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &SSH{client: ssh.NewClient(c, chans, reqs), signalTimeout: 300 * time.Millisecond}
	t.Cleanup(func() { _ = d.Close() })
	p := &sshProcess{driver: d, pid: 4242}

	errCh := make(chan error, 1)
	start := time.Now()
	go func() { errCh <- p.Signal(int(syscall.SIGTERM)) }()
	select {
	case err := <-errCh:
		took := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Signal over a wedged link returned %v after %s, want context.DeadlineExceeded", err, took)
		}
		select {
		case <-execSeen:
		default:
			t.Fatal("the server never saw the signal's exec request, so the bound was not what ended it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Signal over a wedged link still blocked after 10s (bound 300ms)")
	}
}
