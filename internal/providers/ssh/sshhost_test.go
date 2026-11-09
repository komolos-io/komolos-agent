package sshhost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const sample = `### version
komolos-discover 1
### end version rc=0
### os
os_PRETTY_NAME=Ubuntu 22.04.4 LTS
os_ID=ubuntu
os_VERSION_ID=22.04
kernel=5.15.0-105-generic
arch=x86_64
hostname=web-1
cpus=4
memory_kib=8000000
virtualization=kvm
sys_vendor=Amazon EC2
### end os rc=0
### packages
nginx	1.18.0-6ubuntu14.4
postgresql-14	14.11-0ubuntu0.22.04.1
### end packages rc=0
### listeners
tcp LISTEN 0 511 0.0.0.0:80 0.0.0.0:*
tcp LISTEN 0 511 [::]:80 [::]:*
tcp LISTEN 0 244 127.0.0.1:5432 0.0.0.0:* users:(("postgres",pid=812,fd=5))
udp UNCONN 0 0 127.0.0.53%lo:53 0.0.0.0:*
### end listeners rc=0
### connections
0 0 10.0.1.5:80 41.58.1.2:51234
0 0 10.0.1.5:80 41.58.1.3:51990
0 0 10.0.1.5:44012 10.0.2.9:5432
0 0 127.0.0.1:41000 127.0.0.1:6379
### end connections rc=0
### processes
root systemd
www-data nginx
www-data nginx
postgres postgres
root kworker/0:1
### end processes rc=0
### services
nginx.service loaded active running A high performance web server
postgresql@14-main.service loaded active running PostgreSQL Cluster 14-main
### end services rc=0
### mounts
Filesystem Type 1-blocks Used Available Capacity Mounted on
/dev/nvme0n1p1 ext4 20000000000 5000000000 15000000000 25% /
### end mounts rc=0
### disks
nvme0n1 21474836480 disk 0
loop0 0 loop 0
### end disks rc=0
### containers
# unavailable: containers
### end containers rc=0
`

func TestParse(t *testing.T) {
	h, gaps := Parse("10.0.1.5", sample)
	if h.WrapperVersion != "1" || h.Hostname != "web-1" || h.OS.ID != "ubuntu" || h.CPUs != 4 || h.MemoryBytes != 8000000*1024 {
		t.Fatalf("os fields: %+v", h)
	}
	if len(h.Packages) != 2 || h.Packages[1].Name != "postgresql-14" {
		t.Fatalf("packages: %+v", h.Packages)
	}
	if len(h.Listeners) != 4 {
		t.Fatalf("listeners: %+v", h.Listeners)
	}
	var pg bool
	for _, l := range h.Listeners {
		if l.Port == 5432 && l.Process == "postgres" && l.Address == "127.0.0.1" {
			pg = true
		}
		if l.Port == 53 && l.Address != "127.0.0.53" {
			t.Errorf("zone suffix not stripped: %+v", l)
		}
		if l.Port == 80 && l.Process != "" {
			t.Errorf("process must stay unknown when ss does not show it: %+v", l)
		}
	}
	if !pg {
		t.Errorf("postgres listener not found: %+v", h.Listeners)
	}
	want := map[string]int{"inbound:80": 2, "10.0.2.9:5432": 1, "loopback:6379": 1}
	for _, c := range h.Connections {
		key := c.Peer + ":" + strconv.Itoa(c.Port)
		if want[key] != c.Count {
			t.Errorf("connection %s count %d", key, c.Count)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("missing connections: %v", want)
	}
	for _, p := range h.Processes {
		if strings.HasPrefix(p.Name, "kworker") {
			t.Errorf("kernel thread kept: %+v", p)
		}
		if p.Name == "nginx" && p.Count != 2 {
			t.Errorf("nginx count %d", p.Count)
		}
	}
	if len(h.Services) != 2 || len(h.Mounts) != 1 || len(h.Disks) != 1 {
		t.Errorf("services/mounts/disks: %v %v %v", h.Services, h.Mounts, h.Disks)
	}
	if len(gaps) != 1 || gaps[0].Code != "unavailable:containers" {
		t.Errorf("gaps: %+v", gaps)
	}
}

func TestParseFailedAndMissingSections(t *testing.T) {
	out := "### version\nkomolos-discover 1\n### end version rc=0\n### services\nFailed to connect to bus\n### end services rc=1\n### packages\nnginx\t1\n"
	_, gaps := Parse("h", out)
	codes := map[string]string{}
	for _, g := range gaps {
		codes[g.Area] = g.Code
	}
	if codes["services"] != "exit_1" || codes["packages"] != "truncated" || codes["os"] != "missing" {
		t.Errorf("gaps: %+v", gaps)
	}
}

func TestParseTarget(t *testing.T) {
	good := map[string]Target{
		"10.0.0.5":         {Host: "10.0.0.5"},
		"ops@db1:2222":     {Host: "db1", Port: 2222, User: "ops"},
		"[2001:db8::1]:22": {Host: "2001:db8::1", Port: 22},
		"2001:db8::1":      {Host: "2001:db8::1"},
		"web.internal.lan": {Host: "web.internal.lan"},
	}
	for in, want := range good {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("ParseTarget(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"-oProxyCommand=x", "a@-b", "host;rm", "h:notaport", "x@"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) accepted", bad)
		}
	}
}

// fakeSSH stands in for the OpenSSH client: it runs the real wrapper locally
// with the last argument as SSH_ORIGINAL_COMMAND, exactly as sshd would.
func fakeSSH(t *testing.T, behaviour string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper test needs a POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	// The wrapper lives at the repository root so reviewers find it first.
	wrapper, _ := filepath.Abs("../../../komolos-discover.sh")
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh")
	var body string
	switch behaviour {
	case "wrapper":
		body = "for a; do last=$a; done\nSSH_ORIGINAL_COMMAND=$last exec sh " + wrapper + "\n"
	case "denied":
		body = "echo 'ops@h: Permission denied (publickey).' >&2\nexit 255\n"
	case "noforce":
		body = "echo 'sh: 1: all: not found' >&2\nexit 127\n"
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCollectEndToEndWithRealWrapper(t *testing.T) {
	cfg := Config{SSHBinary: fakeSSH(t, "wrapper"), DefaultUser: "komolos-discovery"}
	hosts, _ := Collect(context.Background(), cfg, []Target{{Host: "localhost"}}, nil)
	h := hosts[0]
	if h.WrapperVersion != WrapperVersion || h.OS.Kernel == "" || len(h.Processes) == 0 || len(h.Mounts) == 0 {
		t.Fatalf("host not populated from real wrapper: %+v", h)
	}
	for _, p := range h.Processes {
		if strings.Contains(p.Name, " -") || strings.Contains(p.Name, "--") {
			t.Errorf("looks like a command line leaked: %q", p.Name)
		}
	}
}

func TestCollectFailures(t *testing.T) {
	for behaviour, want := range map[string]string{"denied": "auth_failed", "noforce": "exit_127"} {
		cfg := Config{SSHBinary: fakeSSH(t, behaviour)}
		_, gaps := Collect(context.Background(), cfg, []Target{{Host: "h"}}, nil)
		if len(gaps) != 1 || gaps[0].Code != want {
			t.Errorf("%s: gaps %+v, want code %s", behaviour, gaps, want)
		}
	}
}

func TestArgsEndOptionsBeforeHost(t *testing.T) {
	cfg := Config{}
	cfg.defaults()
	a := cfg.args(Target{Host: "h", User: "u", Port: 2200})
	n := len(a)
	if a[n-3] != "--" || a[n-2] != "h" || a[n-1] != "all" {
		t.Errorf("args tail: %v", a[n-3:])
	}
	if !strings.Contains(strings.Join(a, " "), "StrictHostKeyChecking=yes") {
		t.Error("host keys must be checked by default")
	}
}
