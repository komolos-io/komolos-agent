# Install komolos-agent on Windows (PowerShell 5.1 or later).
#
#   irm https://raw.githubusercontent.com/komolos-io/komolos-agent/main/install.ps1 | iex
#
# Pin a version first:  $env:KOMOLOS_VERSION = "v0.2.0"
#
# Downloads the matching zip from the GitHub Release, checks its SHA-256
# against checksums.txt, verifies that file's signature if cosign is installed,
# and installs komolos-agent.exe to %LOCALAPPDATA%\Programs\komolos (no
# administrator rights needed), adding that folder to your user PATH. Installs
# nothing if a check fails. The SSH mode needs the Windows OpenSSH Client
# (Settings > Optional features).

& {
    $ErrorActionPreference = "Stop"
    $repo = "komolos-io/komolos-agent"
    $bin = "komolos-agent"

    # Colour only where the console understands ANSI (Windows Terminal, PowerShell 7).
    $vt = $false
    try { $vt = [bool]$Host.UI.SupportsVirtualTerminal } catch { }
    $e = [char]27
    $okMark = if ($vt) { [char]0x2713 } else { "+" }
    function Paint($code, $text) { if ($vt) { "$e[${code}m$text$e[0m" } else { $text } }
    function Row($mark, $label, $value) {
        $m = switch ($mark) { "ok" { Paint "38;5;42;1" $okMark } "skip" { Paint "2" "-" } default { Paint "38;5;203;1" "x" } }
        Write-Host ("  $m  " + $label.PadRight(14) + (Paint "2" $value))
    }

    $banner = @'
    __ ______  __  _______  __    ____  _____
   / //_/ __ \/  |/  / __ \/ /   / __ \/ ___/
  / ,< / / / / /|_/ / / / / /   / / / /\__ \
 / /| / /_/ / /  / / /_/ / /___/ /_/ /___/ /
/_/ |_\____/_/  /_/\____/_____/\____//____/
'@
    $shades = @("38;5;23", "38;5;30", "38;5;37", "38;5;44", "38;5;51")
    $colors = @("DarkCyan", "DarkCyan", "Cyan", "Cyan", "Cyan")
    Write-Host ""
    $i = 0
    foreach ($l in ($banner -split "`r?`n")) {
        if ($vt) { Write-Host ("  " + (Paint "$($shades[$i]);1" $l)) } else { Write-Host "  $l" -ForegroundColor $colors[$i] }
        $i++
    }
    Write-Host ""
    Write-Host ("  " + (Paint "1" "komolos-agent installer"))
    Write-Host ""

    $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { "amd64" }
        "ARM64" { "arm64" }
        default { throw "unsupported CPU: $env:PROCESSOR_ARCHITECTURE" }
    }
    $version = if ($env:KOMOLOS_VERSION) { $env:KOMOLOS_VERSION } else { "latest" }
    $base = if ($version -eq "latest") {
        "https://github.com/$repo/releases/latest/download"
    } else {
        "https://github.com/$repo/releases/download/$version"
    }
    $archive = "${bin}_windows_${arch}.zip"

    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $ProgressPreference = "SilentlyContinue"   # the progress bar makes downloads much slower in PowerShell 5.1
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("komolos-" + [Guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp "checksums.txt")
        Row "ok" "Downloaded" "$archive ($version)"

        if (Get-Command cosign -ErrorAction SilentlyContinue) {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt.sigstore.json" -OutFile (Join-Path $tmp "checksums.txt.sigstore.json")
            & cosign verify-blob --bundle (Join-Path $tmp "checksums.txt.sigstore.json") `
                --certificate-oidc-issuer https://token.actions.githubusercontent.com `
                --certificate-identity-regexp "^https://github.com/$repo/\.github/workflows/release\.yml@refs/tags/v" `
                (Join-Path $tmp "checksums.txt") 2>$null | Out-Null
            if ($LASTEXITCODE -ne 0) { throw "signature check FAILED for checksums.txt; nothing installed" }
            Row "ok" "Signature" "signed by the $repo release workflow"
        } else {
            Row "skip" "Signature" "not checked (install cosign to check it)"
        }

        $line = Get-Content (Join-Path $tmp "checksums.txt") | Where-Object { ($_ -split '\s+')[1] -eq $archive }
        if (-not $line) { throw "$archive is not listed in checksums.txt" }
        $want = ($line -split '\s+')[0].ToLower()
        $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
        if ($want -ne $got) { throw "checksum mismatch for $archive (want $want, got $got); nothing installed" }
        Row "ok" "Checksum" "matches checksums.txt"

        Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force
        $dir = Join-Path $env:LOCALAPPDATA "Programs\komolos"
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        Copy-Item -Force (Join-Path $tmp "$bin.exe") (Join-Path $dir "$bin.exe")

        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        if (($userPath -split ';') -notcontains $dir) {
            [Environment]::SetEnvironmentVariable("Path", "$userPath;$dir", "User")
        }
        if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }   # usable in this window too
        Row "ok" "Installed" "$dir\$bin.exe"
        Write-Host ""
        Write-Host ("  " + (Paint "2" "Try") + "  " + (Paint "38;5;44" "komolos-agent --help"))
        Write-Host ""
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}
