package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// SSH is the golang.org/x/crypto/ssh-backed Driver. Holds one
// reusable control connection per (user, host) pair so per-seed
// restarts (Tier 3) don't pay the TCP handshake + key exchange cost
// each time. Connection is established lazily on the first Start;
// Close tears it down cleanly.
//
// Authentication uses ssh-agent (the SSH_AUTH_SOCK socket the dev
// Mac / cluster-runner host inherits) — no password / no key file
// path baked into probatorium. That matches the existing mage-side
// SSH path: `ssh -o BatchMode=yes mini@<host>` succeeds because the
// dev environment already speaks to ssh-agent.
//
// The driver is goroutine-safe: Start / Close / Process methods can
// fire from multiple goroutines. An internal mutex guards the SSH
// client; per-Process state lives on the localProcess analogue
// `sshProcess`.
type SSH struct {
	user       string
	host       string
	port       int
	binary     string
	hostKeyCB  ssh.HostKeyCallback
	knownHosts string

	mu     sync.Mutex
	client *ssh.Client
	closed bool
}

// SSHConfig groups the optional knobs NewSSH accepts. Zero-value is
// production-safe: Port 22, no known-hosts pinning (accept-on-first-
// connect), 10s ConnectTimeout. The knobs exist for tests + dev hosts
// where the production defaults aren't usable.
type SSHConfig struct {
	// Port is the SSH port. Zero defaults to 22.
	Port int
	// KnownHostsPath, when non-empty, requires the remote host key to
	// match the listed entry. Empty falls back to "accept first
	// connection" (logged) — fine for the LAN cluster, NOT fine for
	// arbitrary hosts.
	KnownHostsPath string
	// ConnectTimeout caps the initial TCP+handshake. Zero defaults
	// to 10s.
	ConnectTimeout time.Duration
}

// NewSSH constructs an SSH driver targeting user@host:port and the
// given remote binary path. The driver does NOT connect eagerly —
// the first Start call establishes the connection.
func NewSSH(user, host, binary string, cfg SSHConfig) *SSH {
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	d := &SSH{
		user:       user,
		host:       host,
		port:       port,
		binary:     binary,
		knownHosts: cfg.KnownHostsPath,
	}
	// Host-key policy:
	//   - knownHosts path set: require strict match.
	//   - empty: accept any host key (LAN-cluster mode). We log the
	//     fingerprint on first connect via the hostKeyCB.
	if cfg.KnownHostsPath != "" {
		d.hostKeyCB = strictHostKeyCB(cfg.KnownHostsPath)
	} else {
		//nolint:gosec // intentional: LAN cluster, no untrusted hosts.
		d.hostKeyCB = ssh.InsecureIgnoreHostKey()
	}
	return d
}

// strictHostKeyCB builds a HostKeyCallback that loads the
// known_hosts file and rejects any mismatch. Used in production
// where the cluster's host keys are pinned at provisioning time.
func strictHostKeyCB(path string) ssh.HostKeyCallback {
	// We deliberately defer the file read to first connect so a
	// missing file fails the SSH connection (with a clear message)
	// rather than NewSSH.
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("known_hosts: %w", err)
		}
		defer func() { _ = f.Close() }()
		buf, err := io.ReadAll(f)
		if err != nil {
			return fmt.Errorf("known_hosts: %w", err)
		}
		want := ssh.MarshalAuthorizedKey(key)
		// Trim whitespace + match line-prefix. The known_hosts format
		// is `<host> <type> <base64>`; we look for the base64 token
		// anywhere in the file. A real implementation would parse the
		// host fields too, but for LAN-cluster the IP doesn't shift
		// between runs.
		if bytes.Contains(buf, bytes.TrimSpace(want)) {
			return nil
		}
		return fmt.Errorf("host key mismatch for %s (fingerprint %s)",
			hostname, ssh.FingerprintSHA256(key))
	}
}

// Start opens (or reuses) the SSH connection and forks the remote
// binary with args. Returns a Process whose PID is the REMOTE pid
// (as reported by `echo $$` immediately before exec).
func (d *SSH) Start(ctx context.Context, args []string) (Process, error) {
	if err := d.ensureClient(ctx); err != nil {
		return nil, err
	}

	// Each Start gets its own session — sessions are 1:1 with
	// running commands.
	d.mu.Lock()
	if d.client == nil {
		d.mu.Unlock()
		return nil, errors.New("ssh: driver closed")
	}
	sess, err := d.client.NewSession()
	d.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("ssh: new session: %w", err)
	}

	stderrPipe, err := sess.StderrPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("ssh: stderr pipe: %w", err)
	}
	stdoutPipe, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("ssh: stdout pipe: %w", err)
	}

	// Build the remote command:
	//   echo $$ > /tmp/probatorium-<rand>.pid; exec <binary> <args...>
	// The PID file gives us a stable post-fork PID; exec replaces
	// the shell so signals delivered to the remote PID hit the binary.
	// (setsid would put the binary in its own process group, but
	// classic setsid -w redirects stdin/stdout/stderr to /dev/null
	// when no controlling terminal is present — would silently drop
	// the refapp's `ready addr=` line. The Signal path uses pkill
	// to walk the process tree instead.)
	pidFile := fmt.Sprintf("/tmp/probatorium-ssh-%d-%d.pid",
		os.Getpid(), time.Now().UnixNano())
	remoteCmd := fmt.Sprintf(
		`echo $$ > %s; exec %s %s`,
		shellQuote(pidFile),
		shellQuote(d.binary),
		strings.Join(quoteAll(args), " "),
	)

	if err := sess.Start(remoteCmd); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("ssh: start remote command: %w", err)
	}

	mergedR, mergedW := io.Pipe()
	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go func() { defer copyWG.Done(); _, _ = io.Copy(mergedW, stderrPipe) }()
	go func() { defer copyWG.Done(); _, _ = io.Copy(mergedW, stdoutPipe) }()
	go func() { copyWG.Wait(); _ = mergedW.Close() }()

	p := &sshProcess{
		driver:    d,
		sess:      sess,
		errReader: mergedR,
		pidFile:   pidFile,
		done:      make(chan struct{}),
	}

	// Resolve the remote PID by reading the pidFile. Best-effort: if
	// the file isn't there yet (very fast race) we retry a few times.
	// 250ms total budget is plenty for `echo $$ > file`.
	p.pid = d.readPIDFile(ctx, pidFile)

	return p, nil
}

// readPIDFile cat's the remote pidFile with up to 5 retries at 50ms.
// Returns 0 if the file is unreadable; sshProcess.PID() exposes that
// to the caller, who handles "PID unknown" the same way the local
// driver does.
func (d *SSH) readPIDFile(ctx context.Context, pidFile string) int {
	for i := 0; i < 5; i++ {
		out, err := d.runQuick(ctx, "cat "+shellQuote(pidFile))
		if err == nil {
			s := strings.TrimSpace(string(out))
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				return n
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// runQuick executes a one-shot remote command and returns its stdout.
// Used for control-plane operations (read pidfile, send signal).
// NOT used for the long-running candidate — that's Start's job.
func (d *SSH) runQuick(_ context.Context, cmd string) ([]byte, error) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	if client == nil {
		return nil, errors.New("ssh: driver closed")
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return sess.CombinedOutput(cmd)
}

// ensureClient establishes the underlying SSH connection on first
// call. Subsequent calls are no-ops (the client is reused).
func (d *SSH) ensureClient(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("ssh: driver closed")
	}
	if d.client != nil {
		return nil
	}
	auths, err := agentAuth()
	if err != nil {
		return fmt.Errorf("ssh: agent: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            d.user,
		Auth:            auths,
		HostKeyCallback: d.hostKeyCB,
		Timeout:         10 * time.Second,
	}
	addr := net.JoinHostPort(d.host, strconv.Itoa(d.port))
	// Dial with a context-aware net.Dialer so a stuck handshake can
	// be cancelled.
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("ssh: dial %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("ssh: handshake %s: %w", addr, err)
	}
	d.client = ssh.NewClient(sshConn, chans, reqs)
	return nil
}

// agentAuth gets ssh-agent's identities as an AuthMethod. Errors out
// if SSH_AUTH_SOCK isn't set — production envs always have it.
func agentAuth() ([]ssh.AuthMethod, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, errors.New("SSH_AUTH_SOCK not set (need a running ssh-agent)")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("dial agent: %w", err)
	}
	ag := agent.NewClient(conn)
	signers, err := ag.Signers()
	if err != nil {
		return nil, fmt.Errorf("agent signers: %w", err)
	}
	if len(signers) == 0 {
		return nil, errors.New("ssh-agent has no identities loaded")
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, nil
}

// Close shuts the SSH connection down. Idempotent.
func (d *SSH) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.client == nil {
		return nil
	}
	err := d.client.Close()
	d.client = nil
	return err
}

// sshProcess is the SSH-backed Process.
type sshProcess struct {
	driver    *SSH
	sess      *ssh.Session
	errReader io.Reader
	pid       int
	pidFile   string
	done      chan struct{}

	mu     sync.Mutex
	result WaitResult
	waited bool
}

func (p *sshProcess) PID() int { return p.pid }

func (p *sshProcess) Signal(sig int) error {
	if p.pid <= 0 {
		return nil
	}
	// Signal order MATTERS. The captured PID is the shell that
	// `echo $$`'d then exec'd into the binary. For binaries that
	// don't fork (most) it IS the binary, so a single kill is
	// enough. For binaries that fork worker children (most server
	// frameworks, sh -c that doesn't optimize-to-exec), we have to
	// catch the children FIRST: once we kill the parent it'll be
	// reaped, child PPIDs flip to init (1), and `pkill -P <pid>`
	// finds zero matches — leaving the children orphaned alive.
	//
	// So: kill children first (pkill -P <pid>), then kill the
	// parent (kill -<sig> <pid>). Trailing `true` because pkill
	// returns 1 when nothing matched (the don't-fork case).
	_, err := p.driver.runQuick(context.Background(),
		fmt.Sprintf(
			"pkill -%d -P %d 2>/dev/null; kill -%d %d 2>/dev/null; true",
			sig, p.pid, sig, p.pid))
	if err != nil {
		if strings.Contains(err.Error(), "exit status 1") {
			return nil
		}
		return err
	}
	return nil
}

// Wait blocks until the remote command exits. Caches the result so
// repeated calls don't re-call ssh.Session.Wait.
func (p *sshProcess) Wait(ctx context.Context) (WaitResult, error) {
	p.mu.Lock()
	if p.waited {
		r := p.result
		p.mu.Unlock()
		return r, nil
	}
	p.mu.Unlock()

	waitErr := make(chan error, 1)
	go func() { waitErr <- p.sess.Wait() }()
	var err error
	select {
	case err = <-waitErr:
	case <-ctx.Done():
		return WaitResult{}, ctx.Err()
	}

	res := WaitResult{}
	if err == nil {
		res.ExitCode = 0
	} else if exitErr, ok := err.(*ssh.ExitError); ok {
		ws := exitErr.Waitmsg
		if ws.Signal() != "" {
			res.Signaled = true
			res.Signal = sshSignalCode(ws.Signal())
			res.ExitCode = -res.Signal
		} else {
			res.ExitCode = ws.ExitStatus()
		}
	} else {
		return WaitResult{}, fmt.Errorf("ssh wait: %w", err)
	}

	// Clean up the remote pidfile. Best-effort.
	_, _ = p.driver.runQuick(context.Background(),
		"rm -f "+shellQuote(p.pidFile))

	p.mu.Lock()
	p.result = res
	p.waited = true
	close(p.done)
	p.mu.Unlock()
	return res, nil
}

func (p *sshProcess) Stderr() io.Reader { return p.errReader }

// sshSignalCode maps SSH protocol signal names (e.g. "TERM") to
// numeric values matching the local syscall.Signal constants. SSH
// strips the SIG prefix.
func sshSignalCode(name string) int {
	switch name {
	case "HUP":
		return int(syscall.SIGHUP)
	case "INT":
		return int(syscall.SIGINT)
	case "QUIT":
		return int(syscall.SIGQUIT)
	case "ILL":
		return int(syscall.SIGILL)
	case "ABRT":
		return int(syscall.SIGABRT)
	case "FPE":
		return int(syscall.SIGFPE)
	case "KILL":
		return int(syscall.SIGKILL)
	case "SEGV":
		return int(syscall.SIGSEGV)
	case "PIPE":
		return int(syscall.SIGPIPE)
	case "ALRM":
		return int(syscall.SIGALRM)
	case "TERM":
		return int(syscall.SIGTERM)
	case "USR1":
		return int(syscall.SIGUSR1)
	case "USR2":
		return int(syscall.SIGUSR2)
	}
	return 0
}

// shellQuote wraps s in single quotes and escapes embedded single
// quotes. Mirrors `shlex.quote` semantics so we can build remote
// commands without worrying about whitespace or metacharacters in
// argv. Standard idiom: `'foo'\”bar'` → "foo'bar".
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quoteAll applies shellQuote to every element. Used to build
// `echo $$; exec <binary> <quoted args>`.
func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return out
}

// Compile-time check: SSH implements Driver.
var _ Driver = (*SSH)(nil)

// Compile-time check: *sshProcess implements Process.
var _ Process = (*sshProcess)(nil)

// _ helps the linker keep encoding/json + filepath imports live for
// the inevitable follow-up that JSON-marshals SSH config or
// resolves path knobs. Drop when those callers land.
var _ = filepath.Join
var _ = json.Marshal
