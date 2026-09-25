$ErrorActionPreference = "Stop"

$Repo = "huilang-me/cfsm-agent"
$GitHubProxy = ""
$InstallVersion = "latest"
$AutoUpdateEnabled = $false

$needValueFor = ""
foreach ($arg in $args) {
    if ($needValueFor) {
        switch ($needValueFor) {
            "proxy" { $GitHubProxy = $arg }
            "version" { $InstallVersion = $arg }
            "auto_update" { $AutoUpdateEnabled = ($arg -eq "1") }
        }
        $needValueFor = ""
        continue
    }
    switch -Regex ($arg) {
        "^--install-ghproxy=(.+)$" { $GitHubProxy = $Matches[1]; continue }
        "^--install-ghproxy$" { $needValueFor = "proxy"; continue }
        "^--install-version=(.+)$" { $InstallVersion = $Matches[1]; continue }
        "^--install-version$" { $needValueFor = "version"; continue }
        "^-auto_update=(.+)$" { $AutoUpdateEnabled = ($Matches[1] -eq "1"); continue }
        "^-auto-update=(.+)$" { $AutoUpdateEnabled = ($Matches[1] -eq "1"); continue }
        "^-auto_update$" { $needValueFor = "auto_update"; continue }
        "^-auto-update$" { $needValueFor = "auto_update"; continue }
    }
}

function Get-ArchName {
    # PROCESSOR_ARCHITECTURE is emulated on Windows ARM64 (amd64 processes
    # under Prism see "AMD64"), so prefer the true native architecture.
    if ($env:PROCESSOR_ARCHITEW6432) {
        switch ($env:PROCESSOR_ARCHITEW6432) {
            "ARM64" { "arm64"; return }
            "ARM64EC" { "arm64"; return }
            "AMD64" { "amd64"; return }
            "x86" { "386"; return }
        }
    }
    try {
        $cpu = Get-CimInstance -ClassName Win32_Processor -ErrorAction Stop | Select-Object -First 1
        # 0=x86 1=MIPS 2=Alpha 6=IA64 9=x64 12=ARM64
        if ($cpu.DeviceArchitecture -eq 12) { "arm64"; return }
        if ($cpu.DeviceArchitecture -eq 9) { "amd64"; return }
    } catch {
        # Fall through to the environment variable below.
    }
    switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { "amd64"; break }
        "ARM64" { "arm64"; break }
        "x86" { "386"; break }
        default { throw "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
    }
}

$command = if ($args.Count -gt 0) { $args[0] } else { "install" }
$payloadArgs = @($args)
if ($command -in @("uninstall", "remove", "delete", "purge")) {
    Write-Host "[INFO] downloading temporary uninstaller"
    $payloadArgs = @($command)
}

$arch = Get-ArchName
$asset = "cf-probe-windows-$arch.exe"
$path = if ($InstallVersion -eq "latest") { "latest/download" } else { "download/$InstallVersion" }
$url = "https://github.com/$Repo/releases/$path/$asset"
if ($GitHubProxy) {
    $url = $GitHubProxy.TrimEnd("/") + "/" + $url
}

$tmp = Join-Path $env:TEMP "cf-probe-bootstrap-$PID.exe"
Write-Host "CF-Server-Monitor Go Probe bootstrap"
Write-Host "  repo    : $Repo"
Write-Host "  version : $InstallVersion"
Write-Host "  target  : windows/$arch"
Write-Host "  asset   : $asset"
Write-Host "  url     : $url"
if ($AutoUpdateEnabled) {
    Write-Warning "Windows auto update downloads and executes cf-probe-update.exe. Antivirus software may block this behavior."
    Write-Warning "If blocked, add these paths to the antivirus allowlist: C:\Program Files\cf-probe\cf-probe.exe, C:\ProgramData\cf-probe\cf-probe-update.exe, C:\ProgramData\cf-probe\cf-probe.cmd"
}

try {
    Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
    if ($args.Count -eq 0) {
        & $tmp install
    } else {
        & $tmp @payloadArgs
    }
    exit $LASTEXITCODE
} finally {
    Remove-Item $tmp -Force -ErrorAction SilentlyContinue
}
