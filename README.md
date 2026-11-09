# komolos-agent

A read-only, touch-and-go inventory tool. Point it at an AWS account or a set
of Linux servers; it writes one JSON file describing what runs there (servers,
networks, volumes, clusters, listening services, which hosts talk to which) and
exits. You read the file and decide what to do with it.

It was built for planning migrations to [komolos](https://komolos.com), but it
needs no komolos account and the file is yours: use it for your own
documentation, audits or any migration. Uploading to komolos is a separate,
optional command.

Despite the name, every mode runs once and exits. There is no daemon, service
or background process, and nothing stays behind except the file you asked for.

## What it does not do

Security teams usually ask these first:

- **Installs nothing persistent.** One static binary. No service, daemon,
  scheduled task, registry key or background process. Delete the binary
  and it is gone; the only thing it leaves is the output file you asked for.
- **Phones home to no one.** No telemetry, analytics, licence check or
  auto-update. Its only network traffic is to the AWS API endpoints or the SSH
  hosts you name, plus the komolos upload endpoint if, and only if, you run
  `--upload`.
- **Changes nothing.** AWS mode uses only Describe, List and Get calls (a test
  fails the build if the code and [`aws-iam-policy.json`](aws-iam-policy.json)
  ever disagree). SSH mode can only run the fixed keywords of a 130-line shell
  script you install yourself and can read.
- **Reads no data.** No file contents, command lines, environment variables,
  database rows or application payloads. Free-text fields are scrubbed of
  secret-looking strings before the file is written.
- **Needs no inbound access.** You run it from inside your network. Nothing
  connects in.

Run `komolos-agent --plan aws` or `komolos-agent --plan ssh --targets hosts.txt`
to see exactly what a run would call, without running it.

The design follows four rules:

1. **The customer starts it, from inside their network.** It runs on one
   machine they choose: a jump host, an admin laptop or a cloud shell. Nothing
   from komolos ever connects in, and no inbound firewall rule is needed.
2. **Read-only, and only metadata.** AWS mode makes only Describe/List/Get
   calls. SSH mode can only run the fixed keywords of a wrapper script the
   customer installs and can read. Neither reads file contents, command lines,
   environment variables or application data.
3. **Review before upload.** The file stays on the customer's machine until
   they run `komolos-agent --upload` with a one-time token.
4. **Unknown, not guessed.** Anything the collector could not read (a missing
   permission, a tool that is not installed) is listed under `gaps`.

## Install

```sh
# Linux, macOS (no root needed; installs to ~/.local/bin)
curl -fsSL https://raw.githubusercontent.com/komolos-io/komolos-agent/main/install.sh | sh

# Windows PowerShell
irm https://raw.githubusercontent.com/komolos-io/komolos-agent/main/install.ps1 | iex

# With Go installed
go install github.com/komolos-io/komolos-agent@latest
```

The installer downloads the archive for your OS and CPU from the GitHub
Release, checks it against `checksums.txt`, and verifies that file's cosign
signature when `cosign` is installed. It installs nothing if a check fails.
Pin a version with `KOMOLOS_VERSION=v0.1.0`, or use a mirror with
`KOMOLOS_DOWNLOAD_BASE=https://...`. Read the script first if you prefer:
it is short.

Files fetched with curl do not get macOS's quarantine flag, so Gatekeeper does
not block them and no paid Apple signing is needed. Test this on a real Mac
before you promise it to customers.

## AWS mode

```sh
komolos-agent --collect aws --region eu-west-1            # or --region eu-west-1,af-south-1
komolos-agent --collect aws --profile prod-readonly --region eu-west-1
```

It uses the standard AWS credential chain (environment variables, shared
config and SSO profiles, instance and container roles). Grant the identity
the read-only policy in [`aws-iam-policy.json`](aws-iam-policy.json).
A missing permission becomes a gap and collection carries on.

Collected per region: VPCs, subnets, route tables, security groups (rules as
declared dependencies), flow-log configuration, EC2 instances (type, platform,
image, placement, attached volumes, whether a public IP exists), EBS volumes,
EKS clusters and ECS clusters. Only the `Name` tag is read, and it is passed
through redaction.

## SSH mode (Linux servers anywhere)

```sh
komolos-agent --collect ssh --targets hosts.txt              # one [user@]host[:port] per line
komolos-agent --collect ssh --host 10.0.1.5 --host ops@db1:2222 -J bastion.example.com
```

It runs the system OpenSSH client, so the customer's own `~/.ssh/config`,
`ProxyJump` bastions, SSH certificates, agent and `known_hosts` all apply.
Unknown host keys are refused unless `--accept-new-host-keys` is passed.

### Setting up each target server (done by the customer's admin)

1. Get the wrapper and read it:

   ```sh
   komolos-agent --print-wrapper > komolos-discover
   ```

   It is the same file as [`komolos-discover.sh`](komolos-discover.sh) in this
   repository and in every release archive (a test checks they are identical).

2. Install it where only root can write. The forced command below runs this
   file, so if the discovery account could edit it, whoever holds the key
   could change what runs:

   ```sh
   sudo install -o root -g root -m 0755 komolos-discover /usr/local/bin/komolos-discover
   ```

   `/usr/local/bin` is only the conventional place for software an admin
   installs by hand. Any root-owned path the discovery account cannot write
   works; it does not need to be on anyone's PATH. This path is on the target
   server and is unrelated to where the komolos source code is hosted.

3. Create a dedicated account with no sudo and no password:

   ```sh
   sudo useradd --create-home --shell /bin/sh komolos-discovery
   sudo passwd -l komolos-discovery
   ```

4. Pin the key to the wrapper in `~komolos-discovery/.ssh/authorized_keys`:

   ```
   restrict,command="/usr/local/bin/komolos-discover",from="<collector-ip>" ssh-ed25519 AAAA... komolos-discovery
   ```

   `restrict` turns off PTY allocation and port, agent and X11 forwarding.
   `command=` runs the wrapper whatever the client asks for; the requested
   keyword arrives as `SSH_ORIGINAL_COMMAND`, and anything outside the
   allow-list exits 126 without running. `from=` limits the key to the
   collector's address.

   Prefer short-lived SSH certificates over a long-lived key: an OpenSSH CA can
   sign the key with `-O force-command=/usr/local/bin/komolos-discover`,
   `-O source-address=<collector-ip>` and `-V +4h`, so access ends by itself.

5. Optional, for container discovery only: one sudoers line that allows
   exactly one command. Check how your distribution's sudo matches arguments
   before relying on it:

   ```
   komolos-discovery ALL=(root) NOPASSWD: /usr/bin/docker ps --format {{.ID}} {{.Image}} {{.Names}} {{.Ports}}
   ```

   Do not add the account to the `docker` group: that is equivalent to root.

6. Remove the authorized_keys line (or let the certificate expire) after the run.

### What the wrapper reads

| Keyword | Source | Notes |
|---|---|---|
| `os` | `/etc/os-release` (3 keys), `uname`, `nproc`, `/proc/meminfo` total, DMI vendor/product | |
| `packages` | `dpkg-query`, `rpm` or `apk` | names and versions |
| `listeners` | `ss -l -n -t -u -p` | owning process shown only for the account's own sockets; others are left unknown |
| `connections` | `ss -n -t state established` | grouped by remote endpoint; inbound clients grouped by local port |
| `processes` | `ps -e -o user=,comm=` | executable names only, never arguments |
| `services` | `systemctl list-units --type=service --state=running` | |
| `mounts`, `disks` | `df -P -T -B1`, `lsblk -d` | |
| `containers` | `sudo -n docker ps --format ...` | only with the sudoers line above |

The collector also redacts secret-looking text (credentials in URLs, AWS keys,
tokens, `password=` pairs, private keys) from every free-text field before
writing the file.

Windows servers need a different mechanism (WinRM with a constrained
PowerShell endpoint) and are not supported in v1. The collector itself runs on
Windows; SSH mode there needs the Windows OpenSSH Client.

## Review, then (optionally) upload

```text
$ komolos-agent --collect aws --region eu-west-1
...
Wrote inventory-aws.json
provider:     aws
account:      123456789012
  eu-west-1      vpcs=2 subnets=6 security_groups=14 instances=9 volumes=11 eks=1 ecs=0
gaps:         1 (reported as unknown, not guessed)
  aws:eu-west-1 flow_logs: UnauthorizedOperation

Review the file before uploading. Nothing has been sent.
```

This step is optional and only for komolos customers. When satisfied, create an upload token in the komolos dashboard (Migrations, New
upload token). It works once and expires after an hour. Then:

```sh
KOMOLOS_UPLOAD_TOKEN=kup_... komolos-agent --upload -f inventory-aws.json
# or: --token-file token.txt, or paste it at the prompt
```

The command shows the file's summary and SHA-256 and asks for confirmation
(`--yes` skips it). The server records the same digest and the collector
checks that it matches.

## Verifying a download by hand

If your policy does not allow piping an installer to a shell:

```sh
V=v0.1.0; F=komolos-agent_linux_amd64.tar.gz
B=https://github.com/komolos-io/komolos-agent/releases/download/$V
curl -fsSLO $B/$F -fsSLO $B/checksums.txt -fsSLO $B/checksums.txt.sigstore.json
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/komolos-io/komolos-agent/\.github/workflows/release\.yml@refs/tags/v' \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar -xzf $F komolos-agent && ./komolos-agent --version
```

Each release also carries an SBOM, and builds are reproducible (`-trimpath`,
fixed timestamps), so you can rebuild from the tagged source and compare.

## Building from source

```sh
go build .            # Go 1.24+; produces ./komolos-agent
go test -race ./...
```

## Releasing

Push a tag; the release workflow runs GoReleaser (`.goreleaser.yaml`):

```sh
git tag v0.1.0 && git push origin v0.1.0
```

It cross-compiles Linux, macOS and Windows for amd64 and arm64 with
`-trimpath`, writes `checksums.txt`, signs it with cosign (keyless, using the
workflow's GitHub OIDC identity), attaches SBOMs, and publishes the release
the installer reads from. Archive names carry no version, so
`releases/latest/download/komolos-agent_linux_amd64.tar.gz` always works.

A checksum from the same place as the binary only catches corruption. The
cosign signature, tied to this repository's release workflow, is what shows
the release was not replaced.

## Later, not v1

A continuous, outbound-only mode for discovery. It needs a relay,
authentication and an ongoing trust relationship, so the run-once, file-based
flow ships first.

## Security and licence

Report vulnerabilities as described in [SECURITY.md](SECURITY.md).
Licensed under the [Apache License 2.0](LICENSE).
