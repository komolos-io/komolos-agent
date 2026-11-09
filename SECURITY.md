# Security policy

## Reporting a vulnerability

Please report security issues privately through GitHub: open the
**Security** tab of this repository and choose **Report a vulnerability**.
Do not open a public issue. You will get an acknowledgement within three
working days.

## Scope

In scope: the `komolos-agent` binary, `komolos-discover.sh`, the installers
(`install.sh`, `install.ps1`) and the release pipeline (`.goreleaser.yaml`,
`.github/workflows/`).

Of particular interest:

- any way for a target server, a crafted SSH response or an AWS API response
  to make the collector run commands, write outside its output file, or
  exhaust memory;
- any way to make `komolos-discover.sh` run something other than its fixed
  read-only commands;
- secrets or application data reaching the inventory file;
- weaknesses in release signing or installer verification.

## Supported versions

Only the latest release receives fixes.
