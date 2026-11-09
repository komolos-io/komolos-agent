#!/bin/sh
# komolos-discover: forced-command wrapper for komolos-agent (Linux).
#
# Install on each target server, owned by root and not writable by anyone else:
#   install -o root -g root -m 0755 komolos-discover.sh /usr/local/bin/komolos-discover
#
# Then pin the discovery key to this script in the discovery account's
# ~/.ssh/authorized_keys (one line):
#   restrict,command="/usr/local/bin/komolos-discover",from="<jump-host-ip>" ssh-ed25519 AAAA... komolos-discovery
#
# Whatever the client asks to run arrives in SSH_ORIGINAL_COMMAND. Only the
# keywords below are accepted; anything else exits 126 without running.
#
# What it reads: OS release, kernel, CPU and memory totals, installed package
# names and versions, listening sockets, established TCP peers, process names
# (comm only, never command lines), running services, mounted filesystems,
# block devices, and optionally containers. It never reads file contents,
# command lines, environment variables or application data.
#
# Read this file before installing it. It is short on purpose.

set -u
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
umask 077
LC_ALL=C
export LC_ALL

KOMOLOS_DISCOVER_VERSION=1

have() { command -v "$1" >/dev/null 2>&1; }
unavailable() { echo "# unavailable: $1"; }

s_version() {
	echo "komolos-discover $KOMOLOS_DISCOVER_VERSION"
}

s_os() {
	if [ -r /etc/os-release ]; then
		# Only these three keys; values are printed as-is.
		grep -E '^(ID|VERSION_ID|PRETTY_NAME)=' /etc/os-release | sed 's/^/os_/; s/"//g'
	fi
	echo "kernel=$(uname -r)"
	echo "arch=$(uname -m)"
	echo "hostname=$(uname -n)"
	if have nproc; then echo "cpus=$(nproc)"; fi
	if [ -r /proc/meminfo ]; then
		awk '/^MemTotal:/ { print "memory_kib=" $2 }' /proc/meminfo
	fi
	if have systemd-detect-virt; then echo "virtualization=$(systemd-detect-virt 2>/dev/null || true)"; fi
	for f in sys_vendor product_name; do
		if [ -r "/sys/class/dmi/id/$f" ]; then echo "$f=$(cat "/sys/class/dmi/id/$f")"; fi
	done
}

s_packages() {
	if have dpkg-query; then
		dpkg-query -W -f='${Package}\t${Version}\n'
	elif have rpm; then
		rpm -qa --qf '%{NAME}\t%{VERSION}-%{RELEASE}\n'
	elif have apk; then
		apk info -v 2>/dev/null
	else
		unavailable "package manager"
	fi
}

s_listeners() {
	# Without root, ss shows the owning process only for this account's own sockets.
	if have ss; then ss -H -l -n -t -u -p; else unavailable ss; fi
}

s_connections() {
	if have ss; then ss -H -n -t state established; else unavailable ss; fi
}

s_processes() {
	# comm is the executable name only; arguments are never read.
	ps -e -o user=,comm=
}

s_services() {
	if have systemctl; then
		systemctl list-units --type=service --state=running --no-legend --no-pager --plain
	else
		unavailable systemctl
	fi
}

s_mounts() {
	df -P -T -B1 -x tmpfs -x devtmpfs -x squashfs -x overlay 2>/dev/null || df -P -T -B1
}

s_disks() {
	if have lsblk; then lsblk -d -n -b -o NAME,SIZE,TYPE,ROTA; else unavailable lsblk; fi
}

s_containers() {
	# Needs exactly one sudoers line for the discovery account, for example:
	#   komolos-discovery ALL=(root) NOPASSWD: /usr/bin/docker ps --format {{.ID}} {{.Image}} {{.Names}} {{.Ports}}
	# Without it, containers are reported as unavailable. Do not add the
	# account to the docker group: that is equivalent to root.
	if have docker && sudo -n /usr/bin/docker ps --format '{{.ID}} {{.Image}} {{.Names}} {{.Ports}}' 2>/dev/null; then
		:
	else
		unavailable containers
	fi
}

SECTIONS="version os packages listeners connections processes services mounts disks containers"

run_all() {
	for s in $SECTIONS; do
		echo "### $s"
		"s_$s"
		echo "### end $s rc=$?"
	done
}

cmd=${SSH_ORIGINAL_COMMAND:-}
case "$cmd" in
	all) run_all ;;
	version | os | packages | listeners | connections | processes | services | mounts | disks | containers) "s_$cmd" ;;
	*)
		echo "komolos-discover: refused: only these keywords are allowed: all $SECTIONS" >&2
		exit 126
		;;
esac
