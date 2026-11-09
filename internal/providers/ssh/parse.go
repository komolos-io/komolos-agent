package sshhost

import (
	"bufio"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/komolos-io/komolos-agent/internal/inventory"
	"github.com/komolos-io/komolos-agent/internal/redact"
)

// WrapperVersion is the komolos-discover version this collector understands.
const WrapperVersion = "1"

// section is one "### name" ... "### end name rc=N" block of wrapper output.
type section struct {
	lines []string
	rc    int
	ended bool
}

func splitSections(out string) map[string]*section {
	secs := map[string]*section{}
	var cur *section
	var curName string
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if rest, ok := strings.CutPrefix(line, "### end "); ok && cur != nil {
			name, rcText, _ := strings.Cut(rest, " rc=")
			if name == curName {
				cur.rc, _ = strconv.Atoi(rcText)
				cur.ended = true
				cur = nil
				continue
			}
		}
		if name, ok := strings.CutPrefix(line, "### "); ok {
			curName = strings.TrimSpace(name)
			cur = &section{}
			secs[curName] = cur
			continue
		}
		if cur != nil {
			cur.lines = append(cur.lines, line)
		}
	}
	return secs
}

// Parse turns the output of "komolos-discover all" into a Host. Sections that
// are missing, failed, or report a tool as unavailable become Gaps.
func Parse(address, out string) (inventory.Host, []inventory.Gap) {
	h := inventory.Host{Address: address}
	scope := "host:" + address
	var gaps []inventory.Gap
	secs := splitSections(out)

	get := func(name string) []string {
		s, ok := secs[name]
		switch {
		case !ok:
			gaps = append(gaps, inventory.Gap{Scope: scope, Area: name, Code: "missing"})
			return nil
		case !s.ended:
			gaps = append(gaps, inventory.Gap{Scope: scope, Area: name, Code: "truncated"})
		case s.rc != 0:
			gaps = append(gaps, inventory.Gap{Scope: scope, Area: name, Code: fmt.Sprintf("exit_%d", s.rc)})
			return nil
		}
		var lines []string
		for _, l := range s.lines {
			if what, ok := strings.CutPrefix(l, "# unavailable: "); ok {
				gaps = append(gaps, inventory.Gap{Scope: scope, Area: name, Code: "unavailable:" + what})
				continue
			}
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		return lines
	}

	if v := get("version"); len(v) > 0 {
		h.WrapperVersion = strings.TrimPrefix(strings.TrimSpace(v[0]), "komolos-discover ")
		if h.WrapperVersion != WrapperVersion {
			gaps = append(gaps, inventory.Gap{Scope: scope, Area: "version", Code: "wrapper_version_" + h.WrapperVersion})
		}
	}
	parseOS(&h, get("os"))
	h.Packages = parsePackages(get("packages"))
	h.Listeners = parseListeners(get("listeners"))
	h.Connections = parseConnections(get("connections"))
	h.Processes = parseProcesses(get("processes"))
	h.Services = parseServices(get("services"))
	h.Mounts = parseMounts(get("mounts"))
	h.Disks = parseDisks(get("disks"))
	h.Containers = parseContainers(get("containers"))
	return h, gaps
}

func parseOS(h *inventory.Host, lines []string) {
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		v = redact.String(strings.TrimSpace(v))
		switch k {
		case "os_ID":
			h.OS.ID = v
		case "os_VERSION_ID":
			h.OS.VersionID = v
		case "os_PRETTY_NAME":
			h.OS.PrettyName = v
		case "kernel":
			h.OS.Kernel = v
		case "arch":
			h.OS.Arch = v
		case "hostname":
			h.Hostname = v
		case "cpus":
			h.CPUs, _ = strconv.Atoi(v)
		case "memory_kib":
			n, _ := strconv.ParseInt(v, 10, 64)
			h.MemoryBytes = n * 1024
		case "virtualization":
			h.Virtualization = v
		case "sys_vendor":
			h.Vendor = v
		case "product_name":
			h.Product = v
		}
	}
}

func parsePackages(lines []string) []inventory.Package {
	out := []inventory.Package{}
	for _, l := range lines {
		if name, ver, ok := strings.Cut(l, "\t"); ok {
			out = append(out, inventory.Package{Name: name, Version: ver})
			continue
		}
		// apk info -v prints name-version-rN on one line.
		if i := apkVersion.FindStringIndex(l); i != nil {
			out = append(out, inventory.Package{Name: l[:i[0]], Version: l[i[0]+1:]})
		}
	}
	return out
}

var apkVersion = regexp.MustCompile(`-\d[^-]*-r\d+$`)

// splitHostPort handles ss forms: 0.0.0.0:22, [::]:22, *:22, 127.0.0.53%lo:53.
func splitHostPort(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, false
	}
	host, portText := s[:i], s[i+1:]
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if j := strings.Index(host, "%"); j >= 0 {
		host = host[:j]
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, false
	}
	return host, port, true
}

var ssProcess = regexp.MustCompile(`users:\(\("([^"]+)"`)

// parseListeners reads "ss -H -l -n -t -u -p":
// netid state recv-q send-q local peer [users:(("name",pid=..,fd=..))]
func parseListeners(lines []string) []inventory.Listener {
	out := []inventory.Listener{}
	seen := map[string]bool{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		addr, port, ok := splitHostPort(f[4])
		if !ok {
			continue
		}
		li := inventory.Listener{Protocol: f[0], Address: addr, Port: port}
		if m := ssProcess.FindStringSubmatch(l); m != nil {
			li.Process = redact.String(m[1])
		}
		key := fmt.Sprintf("%s|%s|%d|%s", li.Protocol, li.Address, li.Port, li.Process)
		if !seen[key] {
			seen[key] = true
			out = append(out, li)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol+out[i].Address < out[j].Protocol+out[j].Address
	})
	return out
}

// parseConnections reads "ss -H -n -t state established": recv-q send-q local peer.
// Connections are grouped by the remote endpoint. A peer whose port is also a
// local listening port is an inbound client, so it is grouped by our port instead.
func parseConnections(lines []string) []inventory.PeerCount {
	counts := map[[2]string]int{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		_, lport, ok1 := splitHostPort(f[2])
		peer, pport, ok2 := splitHostPort(f[3])
		if !ok1 || !ok2 {
			continue
		}
		// Ephemeral ranges start at 32768 on Linux: a high remote port with a
		// low local port is an inbound client; record it against our port.
		if pport >= 32768 && lport < 32768 {
			counts[[2]string{"inbound", strconv.Itoa(lport)}]++
			continue
		}
		if ip := net.ParseIP(peer); ip != nil && ip.IsLoopback() {
			peer = "loopback"
		}
		counts[[2]string{peer, strconv.Itoa(pport)}]++
	}
	out := make([]inventory.PeerCount, 0, len(counts))
	for k, n := range counts {
		port, _ := strconv.Atoi(k[1])
		out = append(out, inventory.PeerCount{Peer: k[0], Port: port, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Peer < out[j].Peer
	})
	return out
}

// parseProcesses reads "ps -e -o user=,comm=" and counts by (name, user).
func parseProcesses(lines []string) []inventory.ProcessSummary {
	counts := map[[2]string]int{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		name := strings.Join(f[1:], " ")
		// Kernel threads are noise for migration planning.
		if strings.HasPrefix(name, "kworker/") || strings.HasPrefix(name, "ksoftirqd/") ||
			strings.HasPrefix(name, "migration/") || strings.HasPrefix(name, "cpuhp/") {
			continue
		}
		counts[[2]string{redact.String(name), f[0]}]++
	}
	out := make([]inventory.ProcessSummary, 0, len(counts))
	for k, n := range counts {
		out = append(out, inventory.ProcessSummary{Name: k[0], User: k[1], Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].User < out[j].User
	})
	return out
}

// parseServices reads "systemctl list-units ... --plain --no-legend": unit load active sub description.
func parseServices(lines []string) []string {
	out := []string{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) > 0 && strings.HasSuffix(f[0], ".service") {
			out = append(out, redact.String(f[0]))
		}
	}
	sort.Strings(out)
	return out
}

// parseMounts reads "df -P -T -B1": source type size used avail capacity mountpoint.
func parseMounts(lines []string) []inventory.Mount {
	out := []inventory.Mount{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 7 || f[0] == "Filesystem" {
			continue
		}
		size, err1 := strconv.ParseInt(f[2], 10, 64)
		used, err2 := strconv.ParseInt(f[3], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, inventory.Mount{
			Source: redact.String(f[0]), FSType: f[1], SizeBytes: size, UsedBytes: used,
			MountPoint: strings.Join(f[6:], " "),
		})
	}
	return out
}

// parseDisks reads "lsblk -d -n -b -o NAME,SIZE,TYPE,ROTA".
func parseDisks(lines []string) []inventory.Disk {
	out := []inventory.Disk{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 4 {
			continue
		}
		size, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || size == 0 {
			continue
		}
		out = append(out, inventory.Disk{Name: f[0], SizeBytes: size, Type: f[2], Rotational: f[3] == "1"})
	}
	return out
}

// parseContainers reads docker ps --format '{{.ID}} {{.Image}} {{.Names}} {{.Ports}}'.
// ID, image and name never contain spaces; everything after them is the port list.
func parseContainers(lines []string) []inventory.Container {
	out := []inventory.Container{}
	for _, l := range lines {
		f := strings.SplitN(strings.TrimSpace(l), " ", 4)
		if len(f) < 3 {
			continue
		}
		c := inventory.Container{ID: f[0], Image: redact.String(f[1]), Name: redact.String(f[2])}
		if len(f) == 4 {
			c.Ports = redact.String(strings.TrimSpace(f[3]))
		}
		out = append(out, c)
	}
	return out
}
