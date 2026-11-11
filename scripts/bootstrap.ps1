<#
  bootstrap.ps1 - bring a clean Windows 11 machine into the EnvSync environment with ONE command.
  (This file is intentionally ASCII-only to avoid encoding problems in Windows PowerShell 5.1.)

  Run in a regular (non-administrator) PowerShell. Windows will ask for UAC only where an installer needs it.

  Dry run (shows every step, changes nothing):
      powershell -ExecutionPolicy Bypass -File .\scripts\bootstrap.ps1 -DryRun

  Full run on a new machine, paired with the machine called DESKTOP-LS890QQ:
      powershell -ExecutionPolicy Bypass -File .\scripts\bootstrap.ps1 `
          -ManifestFile C:\path\to\envsync.yaml `
          -PeerName DESKTOP-LS890QQ -PeerId <DEVICE-ID-OF-THAT-MACHINE>

  Every step asks for confirmation unless -Yes is given. Installing packages accepts their license agreements
  (winget --accept-package-agreements): that is part of your confirmation of the "install prerequisites" step.
  Nothing here deletes anything. To undo autostart later: envsync uninstall.
#>
[CmdletBinding()]
param(
    [string]$RepoUrl = 'https://github.com/kiyoushii/EnvSync.git',
    [string]$Branch = 'main',
    [string]$ManifestFile = '',                 # path to your envsync.yaml (copied to the agent folder)
    [string]$ManifestRepo = '',                 # or a git URL of the private manifest repository containing envsync.yaml
    [string]$PeerName = '',                     # Tailscale name of the other machine (for pairing)
    [string]$PeerId = '',                       # Syncthing Device ID of the other machine
    [switch]$Yes,                               # do not ask for confirmation
    [switch]$DryRun                             # only print the steps
)

$ErrorActionPreference = 'Stop'
$base = Join-Path $env:LOCALAPPDATA 'EnvSync'
$src  = Join-Path $base 'src'
$exe  = Join-Path $base 'bin\envsync.exe'

function Say([string]$m) { Write-Host "[envsync] $m" }
function Ask([string]$q) {
    if ($Yes -or $DryRun) { return $true }
    return ((Read-Host "$q [y/N]") -match '^(y|yes)$')
}
function Run-Step([string]$title, [scriptblock]$body) {
    Say "== $title"
    if ($DryRun) { Say '   (dry run: not executed)'; return }
    & $body
}
function Refresh-Path {
    $machine = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $user = [Environment]::GetEnvironmentVariable('Path', 'User')
    $env:Path = "$machine;$user"
}
function Has-Command([string]$name) { return [bool](Get-Command $name -ErrorAction SilentlyContinue) }
function Envsync { & $exe @args; if ($LASTEXITCODE -ne 0) { throw "envsync $($args -join ' ') failed (exit $LASTEXITCODE)" } }

Say "Plan: prerequisites -> build agent -> Tailscale login -> manifest -> init -> start -> pair -> apply -> software -> settings -> autostart -> checklist"
if ($DryRun) { Say 'DRY RUN: nothing will be changed.' }

# 1. winget
Run-Step 'Check winget' {
    if (-not (Has-Command 'winget')) { throw 'winget not found. Install "App Installer" from the Microsoft Store and run again.' }
    Say 'winget is available.'
}

# 2. prerequisites
$packages = @('Git.Git', 'GoLang.Go', 'Tailscale.Tailscale', 'Syncthing.Syncthing')
Run-Step "Install prerequisites: $($packages -join ', ')" {
    if (-not (Ask "Install missing packages via winget and accept their license agreements?")) { throw 'Cancelled by user.' }
    foreach ($id in $packages) {
        $found = winget list --id $id --exact --source winget --accept-source-agreements 2>$null | Select-String -SimpleMatch $id
        if ($found) { Say "  $id already installed"; continue }
        Say "  installing $id"
        winget install --id $id --exact --source winget --silent --accept-package-agreements --accept-source-agreements
        if ($LASTEXITCODE -ne 0) { throw "winget install $id failed (exit $LASTEXITCODE)" }
    }
    Refresh-Path
}

# 3. build the agent from source
Run-Step "Build the agent from $RepoUrl ($Branch)" {
    Refresh-Path
    foreach ($tool in 'git', 'go') { if (-not (Has-Command $tool)) { throw "$tool not found in PATH. Open a new PowerShell window and run again." } }
    if (Test-Path (Join-Path $src '.git')) {
        git -C $src fetch --quiet origin $Branch
        git -C $src checkout --quiet $Branch
        git -C $src merge --ff-only --quiet "origin/$Branch"
    } else {
        New-Item -ItemType Directory -Force $base | Out-Null
        git clone --quiet --branch $Branch $RepoUrl $src
    }
    New-Item -ItemType Directory -Force (Split-Path $exe) | Out-Null
    $version = (git -C $src rev-parse --short HEAD)
    Push-Location $src
    try { go build -ldflags "-X main.version=$version" -o $exe ./cmd/envsync } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
    Say "  built $exe (version $version)"
}

# 4. Tailscale login (needs you: a browser window opens)
Run-Step 'Tailscale: sign in' {
    $ts = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'
    $status = & $ts status 2>&1
    if ($LASTEXITCODE -ne 0 -or ($status -match 'Logged out|Stopped|NeedsLogin')) {
        Say '  Tailscale is not connected. A browser window will open: sign in with the SAME account as your other machine.'
        if (-not (Ask 'Run "tailscale up" now?')) { throw 'Cancelled by user.' }
        & $ts up
    }
    Say '  Tailscale is connected.'
}

# 5. manifest
Run-Step 'Install the manifest (envsync.yaml)' {
    $dest = Join-Path $base 'envsync.yaml'
    if ($ManifestRepo) {
        $mdir = Join-Path $base 'manifest-repo'
        if (Test-Path (Join-Path $mdir '.git')) { git -C $mdir pull --quiet --ff-only } else { git clone --quiet $ManifestRepo $mdir }
        $ManifestFile = Join-Path $mdir 'envsync.yaml'
    }
    if (-not $ManifestFile) { Say '  no manifest given (-ManifestFile or -ManifestRepo): skipping. Folders and programs will not be configured.'; return }
    if (-not (Test-Path $ManifestFile)) { throw "manifest not found: $ManifestFile" }
    Copy-Item $ManifestFile $dest -Force
    Envsync manifest check
}

# 6. init
Run-Step 'Create Syncthing keys and config (envsync init)' {
    Envsync init
    Say '  Give the Device ID above and this machine Tailscale name to the other machine, then run "envsync pair" there.'
}

# 7. start the agent in the background
Run-Step 'Start the agent' {
    Start-Process -FilePath $exe -ArgumentList 'run', '--background' -WindowStyle Hidden
    & $exe wait --timeout 90
    if ($LASTEXITCODE -ne 0) { Say '  the agent did not answer in 90 s; check %LOCALAPPDATA%\EnvSync\agent.log (the Windows Firewall may be waiting for your answer: allow PRIVATE networks only).' }
}

# 8. pair + folders
Run-Step 'Pair with the other machine and set up folders' {
    if ($PeerName -and $PeerId) {
        Envsync pair --yes $PeerName $PeerId
        if (Test-Path (Join-Path $base 'envsync.yaml')) { Envsync apply --yes }
    } else {
        Say '  -PeerName / -PeerId not given: pair later with: envsync pair <tailscale-name> <device-id>'
    }
}

# 9. programs and settings (read-only plan first, then apply after confirmation)
Run-Step 'Programs (winget): plan, then install after confirmation' {
    if (-not (Test-Path (Join-Path $base 'envsync.yaml'))) { Say '  no manifest: skipping'; return }
    Envsync software plan
    if (Ask 'Install the missing programs listed above?') { if ($Yes) { Envsync software apply --yes } else { Envsync software apply } }
}
Run-Step 'Settings, appearance, app configs: take them from the reference machine (a rollback snapshot is taken first)' {
    if (-not $PeerName) { Say '  -PeerName not given: skipping (later: envsync settings apply --from <reference machine>)'; return }
    Say '  waiting up to 2 minutes for the state of the reference machine to arrive via Syncthing'
    for ($i = 0; $i -lt 24; $i++) {
        if (Test-Path (Join-Path $base "state\$PeerName")) { break }
        Start-Sleep -Seconds 5
    }
    Envsync settings plan --from $PeerName
    if (Ask "Apply the settings of $PeerName listed above?") {
        if ($Yes) { Envsync settings apply --from $PeerName --yes } else { Envsync settings apply --from $PeerName }
    }
}

# 10. autostart (the agent started in step 7 keeps running; autostart takes over from the next logon)
Run-Step 'Autostart at logon (scheduled task, no administrator needed)' {
    if (Ask 'Enable autostart?') { if ($Yes) { Envsync install --yes } else { & $exe install } }
}

# 11. checklist of what cannot be synced
Run-Step 'Write the manual-setup checklist to the Desktop' {
    $out = Join-Path ([Environment]::GetFolderPath('Desktop')) 'EnvSync-checklist.md'
    & $exe checklist | Set-Content -Path $out -Encoding UTF8
    Say "  $out"
}

Say 'Done. Open the interface: envsync ui   (or the tray icon).'
