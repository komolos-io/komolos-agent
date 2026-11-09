// Package sshhost inventories Linux servers over SSH through the
// komolos-discover forced-command wrapper.
//
// It runs the system OpenSSH client rather than an SSH library, so the
// customer's own ~/.ssh/config, ProxyJump bastions, SSH certificates, agents
// and known_hosts all apply unchanged. The customer starts the run from inside
// their network; nothing connects in from komolos.
package sshhost

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/komolos-io/komolos-agent/internal/inventory"
)

// Target is one server to inventory.
type Target struct {
	Host string
	Port int
	User string
}

func (t Target) String() string {
	s := t.Host
	if t.User != "" {
		s = t.User + "@" + s
	}
	if t.Port != 0 && t.Port != 22 {
		s += ":" + strconv.Itoa(t.Port)
	}
	return s
}

type Config struct {
	SSHBinary      string        // default "ssh"
	DefaultUser    string        // used when a target has no user
	IdentityFile   string        // optional -i
	JumpHost       string        // optional -J
	AcceptNewKeys  bool          // StrictHostKeyChecking=accept-new instead of yes
	ConnectTimeout time.Duration // per connection
	HostTimeout    time.Duration // whole run per host
	Parallel       int
	MaxOutputBytes int64
}

func (c *Config) defaults() {
	if c.SSHBinary == "" {
		c.SSHBinary = "ssh"
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.HostTimeout == 0 {
		c.HostTimeout = 2 * time.Minute
	}
	if c.Parallel <= 0 {
		c.Parallel = 4
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = 8 << 20
	}
}

// ParseTargets reads one target per line: [user@]host[:port]. Blank lines and
// lines starting with # are skipped. IPv6 addresses go in brackets: [2001:db8::1]:22.
func ParseTargets(r io.Reader) ([]Target, error) {
	var out []Target
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		t, err := ParseTarget(strings.Fields(line)[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, t)
	}
	return out, sc.Err()
}

func ParseTarget(s string) (Target, error) {
	var t Target
	if u, rest, ok := strings.Cut(s, "@"); ok {
		t.User, s = u, rest
	}
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.Index(s, "]")
		if end < 0 {
			return t, fmt.Errorf("bad address %q", s)
		}
		t.Host = s[1:end]
		if p, ok := strings.CutPrefix(s[end+1:], ":"); ok {
			n, err := strconv.Atoi(p)
			if err != nil {
				return t, fmt.Errorf("bad port in %q", s)
			}
			t.Port = n
		}
	case strings.Count(s, ":") == 1:
		h, p, _ := strings.Cut(s, ":")
		n, err := strconv.Atoi(p)
		if err != nil {
			return t, fmt.Errorf("bad port in %q", s)
		}
		t.Host, t.Port = h, n
	default:
		t.Host = s
	}
	// Refuse anything ssh could read as an option.
	if t.Host == "" || strings.HasPrefix(t.Host, "-") || strings.HasPrefix(t.User, "-") ||
		strings.ContainsAny(t.Host+t.User, " \t'\"`$;&|<>\\") {
		return t, fmt.Errorf("bad target %q", s)
	}
	if t.Port < 0 || t.Port > 65535 {
		return t, fmt.Errorf("bad port in %q", s)
	}
	return t, nil
}

func (c *Config) args(t Target) []string {
	hostKey := "yes"
	if c.AcceptNewKeys {
		hostKey = "accept-new"
	}
	a := []string{
		"-T", // no PTY; the server's restrict option refuses one anyway
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=" + strconv.Itoa(int(c.ConnectTimeout.Seconds())),
		"-o", "StrictHostKeyChecking=" + hostKey,
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
	}
	if c.IdentityFile != "" {
		a = append(a, "-i", c.IdentityFile, "-o", "IdentitiesOnly=yes")
	}
	if c.JumpHost != "" {
		a = append(a, "-J", c.JumpHost)
	}
	if t.Port != 0 {
		a = append(a, "-p", strconv.Itoa(t.Port))
	}
	user := t.User
	if user == "" {
		user = c.DefaultUser
	}
	if user != "" {
		a = append(a, "-l", user)
	}
	// "--" ends options; the keyword becomes SSH_ORIGINAL_COMMAND on the server.
	return append(a, "--", t.Host, "all")
}

// limitedBuffer stops accepting bytes past max so a misbehaving host cannot
// exhaust the collector's memory.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int64
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := b.max - int64(b.buf.Len())
	if int64(len(p)) > room {
		b.exceeded = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// collectOne runs the wrapper on one host. The host's stderr is never copied
// into the inventory; only a short gap code is recorded.
func (c *Config) collectOne(ctx context.Context, t Target) (inventory.Host, []inventory.Gap) {
	address := t.Host
	if t.Port != 0 && t.Port != 22 {
		address += ":" + strconv.Itoa(t.Port)
	}
	scope := "host:" + address
	ctx, cancel := context.WithTimeout(ctx, c.HostTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.SSHBinary, c.args(t)...)
	stdout := &limitedBuffer{max: c.MaxOutputBytes}
	var stderr limitedBuffer
	stderr.max = 4096
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, &stderr, nil
	err := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return inventory.Host{Address: address}, []inventory.Gap{{Scope: scope, Area: "connection", Code: "timeout"}}
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return inventory.Host{Address: address}, []inventory.Gap{{Scope: scope, Area: "connection", Code: "ssh_not_runnable"}}
	}
	if ee != nil {
		code := ee.ExitCode()
		switch {
		case code == 255: // ssh itself failed: auth, host key, network
			return inventory.Host{Address: address}, []inventory.Gap{{Scope: scope, Area: "connection", Code: sshFailureCode(stderr.buf.String())}}
		case code == 126 && stdout.buf.Len() == 0:
			return inventory.Host{Address: address}, []inventory.Gap{{Scope: scope, Area: "wrapper", Code: "refused"}}
		case stdout.buf.Len() == 0:
			return inventory.Host{Address: address}, []inventory.Gap{{Scope: scope, Area: "wrapper", Code: "exit_" + strconv.Itoa(code)}}
		}
	}
	h, gaps := Parse(address, stdout.buf.String())
	if stdout.exceeded {
		gaps = append(gaps, inventory.Gap{Scope: scope, Area: "output", Code: "truncated_at_limit"})
	}
	if h.WrapperVersion == "" && len(gaps) > 0 {
		// Something answered but it was not the wrapper (e.g. no forced command set).
		gaps = append(gaps, inventory.Gap{Scope: scope, Area: "wrapper", Code: "not_installed_or_not_forced"})
	}
	return h, gaps
}

// sshFailureCode maps well-known OpenSSH messages to short codes.
func sshFailureCode(stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "host key verification failed"), strings.Contains(s, "remote host identification has changed"):
		return "host_key_unverified"
	case strings.Contains(s, "permission denied"):
		return "auth_failed"
	case strings.Contains(s, "connection refused"):
		return "connection_refused"
	case strings.Contains(s, "timed out"):
		return "connect_timeout"
	case strings.Contains(s, "could not resolve"):
		return "dns_failed"
	case strings.Contains(s, "no route to host"), strings.Contains(s, "network is unreachable"):
		return "unreachable"
	default:
		return "ssh_failed"
	}
}

// Collect inventories every target, Parallel at a time, and returns hosts in
// input order. Progress lines go to log (stderr in the CLI).
func Collect(ctx context.Context, cfg Config, targets []Target, log io.Writer) ([]inventory.Host, []inventory.Gap) {
	cfg.defaults()
	if log == nil {
		log = io.Discard
	}
	hosts := make([]inventory.Host, len(targets))
	gaps := make([][]inventory.Gap, len(targets))
	sem := make(chan struct{}, cfg.Parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			h, g := cfg.collectOne(ctx, t)
			hosts[i], gaps[i] = h, g
			mu.Lock()
			status := "ok"
			if len(g) > 0 {
				status = fmt.Sprintf("%d gap(s)", len(g))
			}
			fmt.Fprintf(log, "  %-30s %s\n", t.String(), status)
			mu.Unlock()
		}()
	}
	wg.Wait()
	var all []inventory.Gap
	for _, g := range gaps {
		all = append(all, g...)
	}
	return hosts, all
}

// LookPath reports a clear error when no OpenSSH client is installed.
func LookPath(bin string) error {
	if bin == "" {
		bin = "ssh"
	}
	if _, err := exec.LookPath(bin); err != nil {
		if os.PathSeparator == '\\' {
			return fmt.Errorf("%s not found: install the Windows OpenSSH Client (Settings > Optional features)", bin)
		}
		return fmt.Errorf("%s not found: install the OpenSSH client", bin)
	}
	return nil
}

// Command returns the exact command line the collector runs for a target, so
// a reviewer can see it (komolos-agent --plan ssh) before anything connects.
func (c Config) Command(t Target) []string {
	c.defaults()
	return append([]string{c.SSHBinary}, c.args(t)...)
}

// Keywords are the only requests the wrapper accepts; the collector sends "all".
var Keywords = []string{"version", "os", "packages", "listeners", "connections", "processes", "services", "mounts", "disks", "containers"}
