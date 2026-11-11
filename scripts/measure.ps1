<#
  measure.ps1 - EnvSync environment measurement. READ-ONLY: changes nothing on the system.
  Run in a regular PowerShell (not as administrator) on each of the two machines:
      powershell -ExecutionPolicy Bypass -File .\measure.ps1
  Output is printed and saved to measure-<computer name>.txt next to the script.
  The script sends nothing over the network. The report contains your user name and
  top-level folder names: review it before sharing.
  (This file is intentionally ASCII-only to avoid encoding problems in PowerShell 5.1.)
#>

$ErrorActionPreference = 'SilentlyContinue'
$report = New-Object System.Collections.Generic.List[string]
function Out-Line([string]$s) { $report.Add($s); Write-Host $s }

# Folder names excluded from sync by plan (caches, build folders, .git)
$excluded = @('node_modules', '.git', '__pycache__', 'Cache', 'Code Cache', 'GPUCache', 'Temp')

function Get-SizeGB([string]$path) {
    if (-not (Test-Path -LiteralPath $path)) { return $null }
    $sum = 0L
    $stack = New-Object System.Collections.Generic.Stack[string]
    $stack.Push($path)
    while ($stack.Count -gt 0) {
        $dir = $stack.Pop()
        try {
            foreach ($f in [System.IO.Directory]::EnumerateFiles($dir)) {
                try { $sum += ([System.IO.FileInfo]$f).Length } catch {}
            }
            foreach ($d in [System.IO.Directory]::EnumerateDirectories($dir)) {
                $name = [System.IO.Path]::GetFileName($d)
                $attr = [System.IO.File]::GetAttributes($d)
                if ($attr -band [System.IO.FileAttributes]::ReparsePoint) { continue }  # do not follow links
                if ($excluded -contains $name) { continue }
                $stack.Push($d)
            }
        } catch {}
    }
    return [math]::Round($sum / 1GB, 2)
}

Out-Line "=== EnvSync measure ($(Get-Date -Format s)) ==="
Out-Line "Machine:       $env:COMPUTERNAME"
Out-Line "User:          $env:USERNAME"
Out-Line "Profile:       $env:USERPROFILE"
$os = Get-CimInstance Win32_OperatingSystem
Out-Line "Windows:       $($os.Caption) $($os.Version) (build $($os.BuildNumber))"
$edition = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').EditionID
Out-Line "Edition:       $edition"

Out-Line ""
Out-Line "--- Disks (letter, label, filesystem, size / free, GB) ---"
Get-CimInstance Win32_LogicalDisk | ForEach-Object {
    $size = if ($_.Size) { [math]::Round($_.Size / 1GB, 1) } else { 0 }
    $free = if ($_.FreeSpace) { [math]::Round($_.FreeSpace / 1GB, 1) } else { 0 }
    Out-Line ("{0}  {1,-16} {2,-6} {3,8} / {4,8}" -f $_.DeviceID, $_.VolumeName, $_.FileSystem, $size, $free)
}

Out-Line ""
Out-Line "--- Profile folder sizes, excluding node_modules, .git, caches (GB) ---"
$total = 0.0
Get-ChildItem -LiteralPath $env:USERPROFILE -Directory -Force | ForEach-Object {
    $gb = Get-SizeGB $_.FullName
    if ($null -ne $gb) { $total += $gb; Out-Line ("{0,-30} {1,10}" -f $_.Name, $gb) }
}
Out-Line ("{0,-30} {1,10}" -f 'TOTAL (profile)', [math]::Round($total, 2))
Out-Line "With version-history reserve (+30..50%): $([math]::Round($total*1.3,1)) - $([math]::Round($total*1.5,1)) GB"

Out-Line ""
Out-Line "--- Tools ---"
foreach ($tool in 'winget', 'git', 'tailscale', 'syncthing', 'go') {
    $c = Get-Command $tool -ErrorAction SilentlyContinue
    Out-Line ("{0,-10} {1}" -f $tool, $(if ($c) { $c.Source } else { 'not found' }))
}
$sandbox = (Get-WindowsOptionalFeature -Online -FeatureName 'Containers-DisposableClientVM').State
Out-Line ("Windows Sandbox: {0}" -f $(if ($sandbox) { $sandbox } else { 'unavailable' }))

$out = Join-Path $PSScriptRoot ("measure-{0}.txt" -f $env:COMPUTERNAME)
$report | Set-Content -LiteralPath $out -Encoding UTF8
Write-Host "`nReport saved: $out"
