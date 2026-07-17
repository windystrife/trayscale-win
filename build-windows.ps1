# Build script for the Windows editions of Trayscale.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File .\build-windows.ps1
#
# Produces two standalone, no-console binaries in dist\:
#   Trayscale-GUI.exe  - full window (Gio), machine sidebar + detail pane
#   Trayscale.exe      - system-tray icon + menu (fyne.io/systray)
#
# Both reuse the same tsutil core and need no CGO / GTK. Requires Go >= 1.26.5.

$ErrorActionPreference = 'Stop'

# Point this at your Go install if `go` is not already on PATH.
$GoBin = 'C:\Users\tungnt\sdk\go\bin'
if (Test-Path $GoBin) { $env:Path = "$GoBin;$env:Path" }

$repo = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $repo

$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'

New-Item -ItemType Directory -Path (Join-Path $repo 'dist') -Force | Out-Null

# Each main package needs an embedded Windows resource (.syso) holding BOTH the
# app manifest (ID 1) and the app icon (RT_GROUP_ICON ID 1, which Gio loads for
# the window icon). Without a supportedOS compatibility block in the manifest,
# tailscale.com's version detection panics with "incoherent Windows version".
# go-winres builds a single .syso with both at ID 1 (different resource types);
# rsrc can't (it numbers the manifest 1 and the icon 2). Regenerate if missing.
function Ensure-Syso($cmdDir) {
    $syso = Join-Path $repo "$cmdDir\rsrc_windows_amd64.syso"
    $winres = Join-Path $repo "$cmdDir\winres.json"
    if (-not (Test-Path $syso)) {
        Write-Host "Generating manifest+icon resource for $cmdDir ..."
        go run github.com/tc-hib/go-winres@latest make --in $winres --arch amd64 --out (Join-Path $repo "$cmdDir\rsrc")
    }
}

Ensure-Syso 'cmd\trayscale-gui'
Ensure-Syso 'cmd\trayscale-win'

Write-Host 'Building dist\Trayscale-GUI.exe (Gio window) ...'
go build -mod=mod -trimpath -ldflags '-s -w -H=windowsgui' -o (Join-Path $repo 'dist\Trayscale-GUI.exe') ./cmd/trayscale-gui
if ($LASTEXITCODE -ne 0) { throw "GUI build failed ($LASTEXITCODE)" }

Write-Host 'Building dist\Trayscale.exe (tray) ...'
go build -mod=mod -trimpath -ldflags '-s -w -H=windowsgui' -o (Join-Path $repo 'dist\Trayscale.exe') ./cmd/trayscale-win
if ($LASTEXITCODE -ne 0) { throw "tray build failed ($LASTEXITCODE)" }

foreach ($n in 'Trayscale-GUI.exe', 'Trayscale.exe') {
    $f = Join-Path $repo "dist\$n"
    Write-Host ("OK: {0} ({1:N1} MB)" -f $f, ((Get-Item $f).Length / 1MB)) -ForegroundColor Green
}
