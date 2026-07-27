package qqd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Executor abstracts command and file operations on local or remote hosts.
type Executor interface {
	Run(ctx context.Context, cmd string) (string, error)
	RunStream(ctx context.Context, cmd string, w io.Writer) error
	CopyFrom(ctx context.Context, remotePath, localPath string) error
	CopyTo(ctx context.Context, localPath, remotePath string) error
	Close() error
	ID() string
}

// LocalExecutor runs commands and copies on the current machine.
type LocalExecutor struct{}

// Run executes a shell command locally and returns stdout.
func (LocalExecutor) Run(ctx context.Context, cmd string) (string, error) {
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	var out bytes.Buffer
	var stderr bytes.Buffer
	c.Stdout = &out
	c.Stderr = &stderr
	err := c.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out.String(), nil
}

// RunStream executes a shell command locally, streaming output to w.
func (LocalExecutor) RunStream(ctx context.Context, cmd string, w io.Writer) error {
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Stdout = w
	c.Stderr = w
	return c.Run()
}

// CopyFrom copies a local path to another local path (local mode shortcut).
func (LocalExecutor) CopyFrom(ctx context.Context, remotePath, localPath string) error {
	_, err := LocalExecutor{}.Run(ctx, fmt.Sprintf("cp %s %s", shellQuote(remotePath), shellQuote(localPath)))
	return err
}

// CopyTo copies a local path to another local path (local mode shortcut).
func (LocalExecutor) CopyTo(ctx context.Context, localPath, remotePath string) error {
	_, err := LocalExecutor{}.Run(ctx, fmt.Sprintf("cp %s %s", shellQuote(localPath), shellQuote(remotePath)))
	return err
}

// Close is a no-op for local executor.
func (LocalExecutor) Close() error { return nil }

// ID returns a stable executor identifier.
func (LocalExecutor) ID() string { return "local" }

// SSHExecutor runs commands over a persistent SSH connection.
type SSHExecutor struct {
	client *ssh.Client
	user   string
	host   string
	closed bool
}

// newSSHExecutor dials an SSH connection and returns an executor.
func newSSHExecutor(user, host, keyPath string, port int, insecureHostKey bool) (*SSHExecutor, error) {
	if port == 0 {
		port = 22
	}
	auth, err := sshAuthMethods(keyPath)
	if err != nil {
		return nil, err
	}
	hostKeyCB, err := hostKeyCallback(insecureHostKey)
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hostKeyCB,
		Timeout:         10 * time.Second,
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("SSH connect %s@%s: %w", user, host, err)
	}
	return &SSHExecutor{client: client, user: user, host: host}, nil
}

// hostKeyCallback returns a host key verifier.
// By default, known_hosts is required; callers may opt into insecure mode explicitly.
func hostKeyCallback(insecure bool) (ssh.HostKeyCallback, error) {
	if insecure {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir for known_hosts: %w", err)
	}
	knownHostsPath := filepath.Join(home, ".ssh", "known_hosts")
	return hostKeyCallbackForPath(knownHostsPath, false)
}

// hostKeyCallbackForPath returns a verifier for a specific known_hosts path.
func hostKeyCallbackForPath(knownHostsPath string, insecure bool) (ssh.HostKeyCallback, error) {
	if insecure {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	if _, err := os.Stat(knownHostsPath); err != nil {
		return nil, fmt.Errorf("known_hosts not found at %s (or set target.insecure_host_key = true)", knownHostsPath)
	}
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("parse known_hosts %s: %w", knownHostsPath, err)
	}
	return cb, nil
}

// sshAuthMethods builds authentication methods from key file and/or SSH agent.
func sshAuthMethods(keyPath string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if keyPath != "" {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read SSH key %s: %w", keyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse SSH key %s: %w", keyPath, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH auth available (set ssh_key in config or SSH_AUTH_SOCK)")
	}
	return methods, nil
}

// Run executes a remote shell command and returns stdout.
func (e *SSHExecutor) Run(ctx context.Context, cmd string) (string, error) {
	session, err := e.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close()

	var out, stderr bytes.Buffer
	session.Stdout = &out
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()

	select {
	case err := <-done:
		if err != nil {
			return out.String(), fmt.Errorf("%w: %s", err, stderr.String())
		}
		return out.String(), nil
	case <-ctx.Done():
		session.Close()
		return out.String(), ctx.Err()
	}
}

// RunStream executes a remote command, streaming output to w.
func (e *SSHExecutor) RunStream(ctx context.Context, cmd string, w io.Writer) error {
	session, err := e.client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close()

	session.Stdout = w
	session.Stderr = w

	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		session.Close()
		return ctx.Err()
	}
}

// CopyFrom downloads a remote file to a local path.
func (e *SSHExecutor) CopyFrom(ctx context.Context, remotePath, localPath string) error {
	session, err := e.client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	session.Stdout = f

	done := make(chan error, 1)
	go func() { done <- session.Run(fmt.Sprintf("cat %s", shellQuote(remotePath))) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		session.Close()
		return ctx.Err()
	}
}

// CopyTo uploads a local file to a remote path.
func (e *SSHExecutor) CopyTo(ctx context.Context, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	session, err := e.client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close()

	session.Stdin = f

	done := make(chan error, 1)
	go func() { done <- session.Run(fmt.Sprintf("cat > %s", shellQuote(remotePath))) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		session.Close()
		return ctx.Err()
	}
}

// Close closes the underlying SSH connection. Idempotent, so callers that
// close an executor as soon as they are done with a target can still keep a
// `defer Close()` as a safety net for early returns.
func (e *SSHExecutor) Close() error {
	if e.client == nil || e.closed {
		return nil
	}
	e.closed = true
	return e.client.Close()
}

// ID returns the SSH endpoint identifier.
func (e *SSHExecutor) ID() string {
	return fmt.Sprintf("%s@%s", e.user, e.host)
}

// ExecFactory builds executors for each runtime role.
type ExecFactory interface {
	Local() Executor
	ForTarget(t TargetConfig) (Executor, error)
	ForBuildHost(b BuildConfig) (Executor, error)
}

// DefaultExecFactory is the default executor provider for qqd.
type DefaultExecFactory struct{}

// Local returns the local executor.
func (DefaultExecFactory) Local() Executor {
	return LocalExecutor{}
}

// ForTarget returns local executor for host=local, otherwise dials SSH.
func (DefaultExecFactory) ForTarget(t TargetConfig) (Executor, error) {
	if t.Host == "local" {
		return LocalExecutor{}, nil
	}
	return newSSHExecutor(t.User, t.Host, t.SSHKey, t.SSHPort, t.InsecureHostKey)
}

// ForBuildHost returns local executor for host=local, otherwise dials SSH.
func (DefaultExecFactory) ForBuildHost(b BuildConfig) (Executor, error) {
	if b.Host == "local" {
		return LocalExecutor{}, nil
	}
	return newSSHExecutor(b.User, b.Host, b.SSHKey, b.SSHPort, false)
}
