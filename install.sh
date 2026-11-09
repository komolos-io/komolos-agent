#!/bin/sh
# Install komolos-agent on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/komolos-io/komolos-agent/main/install.sh | sh
#
# Pin a version:     ... | KOMOLOS_VERSION=v0.1.0 sh
# Choose the folder: ... | KOMOLOS_INSTALL_DIR=/opt/komolos/bin sh
# Use a mirror:      ... | KOMOLOS_DOWNLOAD_BASE=https://downloads.example.com/v0.1.0 sh
#
# What it does: detects your OS and CPU, downloads the matching archive from
# the GitHub Release, checks its SHA-256 against the release's checksums.txt,
# verifies the checksums file's signature if cosign is installed, and installs
# one binary. It installs nothing if any check fails. Without root it installs
# to ~/.local/bin; as root, to /usr/local/bin. It never asks for sudo.
#
# Everything runs inside main(), called on the last line, so a partly
# downloaded copy of this script does nothing.

set -eu

REPO="komolos-io/komolos-agent"
BIN="komolos-agent"

say() { printf '%s\n' "$*" >&2; }
fail() {
	say "komolos install: $*"
	exit 1
}
have() { command -v "$1" >/dev/null 2>&1; }

detect_platform() {
	os=$(uname -s)
	case "$os" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	MINGW* | MSYS* | CYGWIN*) fail "on Windows, use PowerShell: irm https://raw.githubusercontent.com/$REPO/main/install.ps1 | iex" ;;
	*) fail "unsupported OS: $os" ;;
	esac
	arch=$(uname -m)
	case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported CPU: $arch" ;;
	esac
	# Rosetta on Apple silicon reports x86_64; prefer the native build.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
		arch=arm64
	fi
}

fetch() { # url dest
	if have curl; then
		curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$2" "$1"
	elif have wget; then
		wget -q --https-only -O "$2" "$1"
	else
		fail "need curl or wget"
	fi
}

sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif have openssl; then
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	else
		fail "need sha256sum, shasum or openssl to verify the download"
	fi
}

main() {
	detect_platform
	version=${KOMOLOS_VERSION:-latest}
	if [ -n "${KOMOLOS_DOWNLOAD_BASE:-}" ]; then # your own mirror of the release files
		base=${KOMOLOS_DOWNLOAD_BASE%/}
	elif [ "$version" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		base="https://github.com/$REPO/releases/download/$version"
	fi
	archive="${BIN}_${os}_${arch}.tar.gz"

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t komolos)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "Downloading $archive ($version)..."
	fetch "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive"
	fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download checksums.txt"

	if have cosign; then
		fetch "$base/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" ||
			fail "could not download the checksums signature"
		cosign verify-blob \
			--bundle "$tmp/checksums.txt.sigstore.json" \
			--certificate-oidc-issuer https://token.actions.githubusercontent.com \
			--certificate-identity-regexp "^https://github.com/$REPO/\.github/workflows/release\.yml@refs/tags/v" \
			"$tmp/checksums.txt" >/dev/null 2>&1 || fail "signature check FAILED for checksums.txt; nothing installed"
		say "Signature verified (cosign, GitHub Actions release workflow)."
	else
		say "cosign not found: checking the SHA-256 only. Install cosign to also verify the signature."
	fi

	want=$(awk -v f="$archive" '$2 == f {print $1}' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
	got=$(sha256_of "$tmp/$archive")
	[ "$want" = "$got" ] || fail "checksum mismatch for $archive (want $want, got $got); nothing installed"
	say "Checksum verified."

	tar -xzf "$tmp/$archive" -C "$tmp" "$BIN" || fail "could not unpack $archive"

	if [ -n "${KOMOLOS_INSTALL_DIR:-}" ]; then
		dir=$KOMOLOS_INSTALL_DIR
	elif [ "$(id -u)" = 0 ]; then
		dir=/usr/local/bin
	else
		dir="$HOME/.local/bin"
	fi
	mkdir -p "$dir"
	# Copy then rename, so a running copy is never half-overwritten.
	cp "$tmp/$BIN" "$dir/.$BIN.new"
	chmod 0755 "$dir/.$BIN.new"
	mv -f "$dir/.$BIN.new" "$dir/$BIN"

	say "Installed $dir/$BIN"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "Add it to your PATH:  export PATH=\"$dir:\$PATH\"" ;;
	esac
	"$dir/$BIN" version >&2 || true
	say "Next: $BIN aws --region <region>   or   $BIN ssh --targets hosts.txt"
}

main "$@"
