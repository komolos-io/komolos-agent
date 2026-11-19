# komolos-agent migration demo (Windows PowerShell 5.1 or 7).
#
#   .\demo.ps1 install   install komolos-agent the way a customer does (signed GitHub release)
#   .\demo.ps1 setup     before the meeting: installs the agent, two local clusters, images, sample app
#   .\demo.ps1 run       in the meeting: show the app, plan, migrate, show it again
#   .\demo.ps1 reset     remove the migrated copy so you can run again
#   .\demo.ps1 cleanup   delete both clusters
#
# Needs Docker Desktop (running), kind and kubectl:
#   winget install Docker.DockerDesktop Kubernetes.kind Kubernetes.kubectl
#
# "source" stands in for a customer's existing cluster (for example EKS);
# "lagos" stands in for the onshore destination. komolos-agent only talks to
# them through kubectl contexts, so the same command works against EKS.
#
# Add -LocalAgent to use a komolos-agent.exe you built yourself instead of the
# published release (for example with no internet).

param(
    [Parameter(Position = 0)][ValidateSet("install", "setup", "run", "reset", "cleanup")][string]$Step = "run",
    [switch]$LocalAgent
)

$ErrorActionPreference = "Stop"
$here = $PSScriptRoot
$src = "kind-source"
$dst = "kind-lagos"
$images = @("nginx:1.27-alpine", "busybox:1.36")

$vt = $false
try { $vt = [bool]$Host.UI.SupportsVirtualTerminal } catch { }
$esc = [char]27
function Say($text) {
    Write-Host ""
    if ($vt) { Write-Host "$esc[38;5;37;1m$([char]0x25B8) $text$esc[0m" } else { Write-Host "> $text" -ForegroundColor Cyan }
}
$installer = "https://raw.githubusercontent.com/komolos-io/komolos-agent/main/install.ps1"

function Install-Agent {
    Say "Installing komolos-agent from its signed GitHub release, as a customer would"
    Write-Host "  irm $installer | iex" -ForegroundColor DarkGray
    Invoke-Expression (Invoke-RestMethod -UseBasicParsing $installer)
}
function Pause($text = "Press Enter to continue") { Write-Host ""; Read-Host $text | Out-Null }
function Need($cmd, $hint) {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) { throw "$cmd not found. $hint" }
}
function Run($exe, [string[]]$argv) {
    & $exe @argv
    if ($LASTEXITCODE -ne 0) { throw "$exe $($argv -join ' ') failed (exit $LASTEXITCODE)" }
}

function Find-Agent {
    if ($LocalAgent) {
        foreach ($p in @("$here\..\..\komolos-agent.exe", "$here\komolos-agent.exe")) {
            if (Test-Path $p) { return (Resolve-Path $p).Path }
        }
        throw "No local komolos-agent.exe. Build it in the komolos-agent folder: go build -o komolos-agent.exe ."
    }
    $installed = Join-Path $env:LOCALAPPDATA "Programs\komolos\komolos-agent.exe"
    if (Test-Path $installed) { return $installed }
    $c = Get-Command komolos-agent -ErrorAction SilentlyContinue
    if ($c) { return $c.Source }
    throw "komolos-agent is not installed. Run .\demo.ps1 install (or add -LocalAgent to use a local build)."
}

function Start-Forward($context, $port) {
    $opts = @{ FilePath = "kubectl"; PassThru = $true
        ArgumentList = @("--context", $context, "-n", "shop", "port-forward", "svc/web", "${port}:80") }
    if ($env:OS -eq "Windows_NT") { $opts.WindowStyle = "Hidden" }   # keep the forward out of sight
    $p = Start-Process @opts
    Start-Sleep -Seconds 3
    return $p
}

function Show-Site($context, $port, $label) {
    $fwd = Start-Forward $context $port
    try {
        $r = Invoke-WebRequest -UseBasicParsing "http://localhost:$port/orders.txt"
        Write-Host "`n$label  http://localhost:$port" -ForegroundColor Green
        Write-Host "served by pod: $($r.Headers['X-Served-By'])"
        Write-Host $r.Content
        if ($env:OS -eq "Windows_NT") { Start-Process "http://localhost:$port" }
    } catch {
        Write-Host "Could not reach the site on $context ($_)" -ForegroundColor Yellow
    }
    return $fwd
}

switch ($Step) {
    "install" { Install-Agent }

    "setup" {
        if (-not $LocalAgent) { Install-Agent }
        Need docker "Install and start Docker Desktop."
        Need kind "winget install Kubernetes.kind"
        Need kubectl "winget install Kubernetes.kubectl"
        docker info *> $null
        if ($LASTEXITCODE -ne 0) { throw "Docker is not running. Start Docker Desktop and try again." }

        Say "Creating two local Kubernetes clusters: source and lagos (about 2 minutes)"
        $have = @(kind get clusters 2>$null)
        foreach ($name in @("source", "lagos")) {
            if ($have -notcontains $name) { Run kind @("create", "cluster", "--name", $name, "--wait", "180s") }
            else { Write-Host "cluster $name already exists" }
        }

        Say "Loading images into both clusters, so the demo needs no internet"
        $platform = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "linux/arm64" } else { "linux/amd64" }
        foreach ($img in $images) {
            # One platform only: kind cannot load multi-platform images from Docker Desktop's containerd store.
            Run docker @("pull", "--platform", $platform, $img)
            foreach ($name in @("source", "lagos")) {
                kind load docker-image $img --name $name
                if ($LASTEXITCODE -ne 0) {
                    Write-Host "Could not preload $img into $name; it will be downloaded instead (needs internet)." -ForegroundColor Yellow
                }
            }
        }

        Say "Deploying the sample store to the source cluster"
        Run kubectl @("--context", $src, "apply", "-f", "$here\app.yaml")
        Run kubectl @("--context", $src, "-n", "shop", "rollout", "status", "deploy/web", "--timeout=180s")
        $pod = (kubectl --context $src -n shop get pods -l app=web -o jsonpath="{.items[0].metadata.name}")

        Say "Putting the store's content and some order records on its volume"
        # Relative paths only: kubectl cp reads "C:" as a pod name.
        Push-Location "$here\site"
        try {
            Run kubectl @("--context", $src, "-n", "shop", "exec", $pod, "--", "mkdir", "-p", "/usr/share/nginx/html/img")
            Get-ChildItem -Recurse -File | ForEach-Object {
                $rel = (Resolve-Path -Relative $_.FullName).TrimStart(".", "\", "/") -replace "\\", "/"
                Run kubectl @("--context", $src, "cp", $rel, "shop/${pod}:/usr/share/nginx/html/$rel")
            }
        } finally { Pop-Location }
        $stamp = Get-Date -Format "yyyy-MM-dd HH:mm:ss"
        $orders = "Orders recorded on the SOURCE cluster at $stamp`nORD-1001  Indigo adire x2         N37,000  paid`nORD-1002  Kampala wax print x1    N32,000  paid`nORD-1003  Aso-oke gele x3          N72,000  pending"
        $orders | kubectl --context $src -n shop exec -i $pod -- sh -c "cat > /usr/share/nginx/html/orders.txt"
        if ($LASTEXITCODE -ne 0) { throw "writing orders.txt failed" }

        Say "Ready. Rehearse with: .\demo.ps1 run   (then .\demo.ps1 reset to run it again)"
    }

    "run" {
        $agent = Find-Agent
        foreach ($c in @($src, $dst)) {
            kubectl config get-contexts $c *> $null
            if ($LASTEXITCODE -ne 0) { throw "context $c not found. Run .\demo.ps1 setup first." }
        }
        $fwds = @()

        Say "1. A store running on the customer's current cluster (source)"
        $fwds += Show-Site $src 8081 "SOURCE"
        Pause

        Say "2. What komolos-agent would move. Read-only on both clusters; nothing is written."
        & $agent --plan k8s --from $src --to $dst -n shop
        Pause

        Say "3. Migrate: config, secrets, the volume's data and the workload, onto the Lagos cluster"
        $env:KOMOLOS_NO_BANNER = "1"   # the logo was just shown by the plan
        & $agent --migrate k8s --from $src --to $dst -n shop --yes --wait 3m -o "$here\last-migration"
        $code = $LASTEXITCODE
        Remove-Item Env:KOMOLOS_NO_BANNER
        if ($code -ne 0) { Write-Host "komolos-agent exited with $code; see last-migration\report.json" -ForegroundColor Yellow }
        Pause

        Say "4. The same store, now served from Lagos, with the same data"
        $fwds += Show-Site $dst 8082 "LAGOS"
        Write-Host "`nThe source cluster is untouched:" -ForegroundColor Green
        kubectl --context $src -n shop get deploy,pvc
        Pause "Press Enter to finish (stops the port-forwards)"
        $fwds | Where-Object { $_ } | ForEach-Object { Stop-Process -Id $_.Id -ErrorAction SilentlyContinue }
    }

    "reset" {
        Say "Removing the migrated copy from lagos"
        kubectl --context $dst delete namespace shop --ignore-not-found --wait=true
        Write-Host "Done. Run .\demo.ps1 run again."
    }

    "cleanup" {
        Say "Deleting both demo clusters"
        kind delete cluster --name source
        kind delete cluster --name lagos
    }
}
