# Install komolos-agent on Windows (PowerShell 5.1 or later).
#
#   irm https://raw.githubusercontent.com/komolos-io/komolos-agent/main/install.ps1 | iex
#
# Pin a version first:  $env:KOMOLOS_VERSION = "v0.1.0"
#
# Downloads the matching zip from the GitHub Release, checks its SHA-256
# against checksums.txt, and installs komolos-agent.exe to
# %LOCALAPPDATA%\Programs\komolos (no administrator rights needed), adding that
# folder to your user PATH. Installs nothing if the checksum does not match.
# The SSH mode needs the Windows OpenSSH Client (Settings > Optional features).

& {
    $ErrorActionPreference = "Stop"
    $repo = "komolos-io/komolos-agent"
    $bin = "komolos-agent"

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
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("komolos-" + [Guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading $archive ($version)..."
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp "checksums.txt")

        $line = Get-Content (Join-Path $tmp "checksums.txt") | Where-Object { ($_ -split '\s+')[1] -eq $archive }
        if (-not $line) { throw "$archive is not listed in checksums.txt" }
        $want = ($line -split '\s+')[0].ToLower()
        $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
        if ($want -ne $got) { throw "checksum mismatch for $archive (want $want, got $got); nothing installed" }
        Write-Host "Checksum verified."

        Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force
        $dir = Join-Path $env:LOCALAPPDATA "Programs\komolos"
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        Copy-Item -Force (Join-Path $tmp "$bin.exe") (Join-Path $dir "$bin.exe")

        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        if (($userPath -split ';') -notcontains $dir) {
            [Environment]::SetEnvironmentVariable("Path", "$userPath;$dir", "User")
            Write-Host "Added $dir to your user PATH. Open a new terminal to use it."
        }
        Write-Host "Installed $dir\$bin.exe"
        & (Join-Path $dir "$bin.exe") version
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}
