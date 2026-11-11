<#
  measure.ps1 — замер среды для EnvSync. ТОЛЬКО ЧТЕНИЕ: ничего не меняет в системе.
  Запуск (обычный PowerShell, не от администратора), на каждой из двух машин:
      powershell -ExecutionPolicy Bypass -File .\measure.ps1
  Результат печатается на экран и сохраняется в measure-<имя машины>.txt рядом со скриптом.
  Скрипт не отправляет данные по сети. Файл отчёта содержит имя пользователя и имена
  папок верхнего уровня, проверьте его перед тем как показывать.
#>

$ErrorActionPreference = 'SilentlyContinue'
$report = New-Object System.Collections.Generic.List[string]
function Out-Line([string]$s) { $report.Add($s); Write-Host $s }

# Имена папок, которые по плану исключаются из синка (кэши, сборочные папки, .git)
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
                if ($attr -band [System.IO.FileAttributes]::ReparsePoint) { continue }  # не идём по ссылкам
                if ($excluded -contains $name) { continue }
                $stack.Push($d)
            }
        } catch {}
    }
    return [math]::Round($sum / 1GB, 2)
}

Out-Line "=== EnvSync measure ($(Get-Date -Format s)) ==="
Out-Line "Машина:        $env:COMPUTERNAME"
Out-Line "Пользователь:  $env:USERNAME"
Out-Line "Профиль:       $env:USERPROFILE"
$os = Get-CimInstance Win32_OperatingSystem
Out-Line "Windows:       $($os.Caption) $($os.Version) (сборка $($os.BuildNumber))"
$edition = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').EditionID
Out-Line "Редакция:      $edition"

Out-Line ""
Out-Line "--- Диски (буква, метка, файловая система, размер/свободно, ГБ) ---"
Get-CimInstance Win32_LogicalDisk | ForEach-Object {
    $size = if ($_.Size) { [math]::Round($_.Size / 1GB, 1) } else { 0 }
    $free = if ($_.FreeSpace) { [math]::Round($_.FreeSpace / 1GB, 1) } else { 0 }
    Out-Line ("{0}  {1,-16} {2,-6} {3,8} / {4,8}" -f $_.DeviceID, $_.VolumeName, $_.FileSystem, $size, $free)
}

Out-Line ""
Out-Line "--- Размер папок профиля без исключений (ГБ; без node_modules, .git, кэшей) ---"
$total = 0.0
Get-ChildItem -LiteralPath $env:USERPROFILE -Directory -Force | ForEach-Object {
    $gb = Get-SizeGB $_.FullName
    if ($null -ne $gb) { $total += $gb; Out-Line ("{0,-30} {1,10}" -f $_.Name, $gb) }
}
Out-Line ("{0,-30} {1,10}" -f 'ИТОГО (профиль)', [math]::Round($total, 2))
Out-Line "С запасом на историю версий (+30..50%): $([math]::Round($total*1.3,1)) - $([math]::Round($total*1.5,1)) ГБ"

Out-Line ""
Out-Line "--- Инструменты ---"
foreach ($tool in 'winget', 'git', 'tailscale', 'syncthing', 'go') {
    $c = Get-Command $tool -ErrorAction SilentlyContinue
    Out-Line ("{0,-10} {1}" -f $tool, $(if ($c) { $c.Source } else { 'не найден' }))
}
$sandbox = (Get-WindowsOptionalFeature -Online -FeatureName 'Containers-DisposableClientVM').State
Out-Line ("Windows Sandbox: {0}" -f $(if ($sandbox) { $sandbox } else { 'недоступно' }))

$out = Join-Path $PSScriptRoot ("measure-{0}.txt" -f $env:COMPUTERNAME)
$report | Set-Content -LiteralPath $out -Encoding UTF8
Write-Host "`nОтчёт сохранён: $out"
