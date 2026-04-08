param(
    [ValidateSet("windows", "linux-amd64", "openwrt-mips", "openwrt-mipsle", "openwrt-arm64", "openwrt-armv7")]
    [string]$Target = "windows",
    [string]$OutputDir = "$PSScriptRoot\dist"
)

$ErrorActionPreference = "Stop"

$cmd = Get-Command go -ErrorAction SilentlyContinue
if ($null -ne $cmd) {
    $goExe = $cmd.Source
} else {
    $fallback = Join-Path $env:ProgramFiles "Go\bin\go.exe"
    if (-not (Test-Path $fallback)) {
        throw "Go executable not found in PATH or Program Files. Install Go and ensure tooling is available."
    }
    $goExe = $fallback
}

if (-not (Test-Path $OutputDir)) {
    New-Item -ItemType Directory -Path $OutputDir | Out-Null
}

$targetOutputDir = Join-Path $OutputDir $Target
if (-not (Test-Path $targetOutputDir)) {
    New-Item -ItemType Directory -Path $targetOutputDir | Out-Null
}

$envConfig = @{}
$outputName = "quota-server.exe"

switch ($Target) {
    "windows" {
        $envConfig = @{ GOOS = "windows"; GOARCH = "amd64" }
        $outputName = "quota-server.exe"
    }
    "linux-amd64" {
        $envConfig = @{ GOOS = "linux"; GOARCH = "amd64"; CGO_ENABLED = "0" }
        $outputName = "quota-server"
    }
    "openwrt-mips" {
        $envConfig = @{ GOOS = "linux"; GOARCH = "mips"; GOMIPS = "softfloat"; CGO_ENABLED = "0" }
        $outputName = "quota-server"
    }
    "openwrt-mipsle" {
        $envConfig = @{ GOOS = "linux"; GOARCH = "mipsle"; GOMIPS = "softfloat"; CGO_ENABLED = "0" }
        $outputName = "quota-server"
    }
    "openwrt-arm64" {
        $envConfig = @{ GOOS = "linux"; GOARCH = "arm64"; CGO_ENABLED = "0" }
        $outputName = "quota-server"
    }
    "openwrt-armv7" {
        $envConfig = @{ GOOS = "linux"; GOARCH = "arm"; GOARM = "7"; CGO_ENABLED = "0" }
        $outputName = "quota-server"
    }
    default {
        throw "Unsupported target: $Target"
    }
}

$previous = @{}
foreach ($key in $envConfig.Keys) {
    $previous[$key] = [Environment]::GetEnvironmentVariable($key, "Process")
    [Environment]::SetEnvironmentVariable($key, $envConfig[$key], "Process")
}

Push-Location $PSScriptRoot
try {
    & $goExe mod tidy

    $outPath = Join-Path $targetOutputDir $outputName
    & $goExe build -trimpath -ldflags "-s -w" -o $outPath ./cmd/server

    Write-Host "Built $Target binary: $outPath"
}
finally {
    Pop-Location
    foreach ($key in $envConfig.Keys) {
        [Environment]::SetEnvironmentVariable($key, $previous[$key], "Process")
    }
}
