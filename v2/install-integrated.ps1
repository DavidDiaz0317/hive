[CmdletBinding()]
param(
    [string]$Version = "latest",
    [string]$Repository = "DavidDiaz0317/hive",
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "Hive"),
    [string]$ReleaseDir = "",
    [switch]$SkipAttestation,
    [switch]$NoPath,
    [switch]$NoCodexSkill
)

function Install-Hive {
$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if (-not [Environment]::Is64BitOperatingSystem) {
    throw "The integrated Hive installer currently supports Windows x64 only."
}
if (-not $ReleaseDir -and -not (Get-Command gh -ErrorAction SilentlyContinue)) {
    throw "GitHub CLI is required for signed artifact verification and the one-time GitHub authorization flow. Install it from https://cli.github.com/."
}
if ($Version -eq "latest") {
    if ($ReleaseDir) { throw "A concrete --Version is required with --ReleaseDir." }
    $Version = (gh api "repos/$Repository/releases/latest" --jq .tag_name).Trim()
    if (-not $Version) { throw "No published integrated Hive release was found." }
}

$asset = "hive-integrated-$Version-windows-amd64.zip"
$checksumAsset = "$asset.sha256"
$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("hive-install-" + [guid]::NewGuid().ToString("N"))
$downloadDir = Join-Path $tempRoot "download"
$extractDir = Join-Path $tempRoot "extract"
$stagingDir = "$InstallDir.new-$([guid]::NewGuid().ToString('N'))"
$backupDir = "$InstallDir.previous"

try {
    New-Item -ItemType Directory -Path $downloadDir, $extractDir -Force | Out-Null
    if ($ReleaseDir) {
        Copy-Item -LiteralPath (Join-Path $ReleaseDir $asset) -Destination $downloadDir
        Copy-Item -LiteralPath (Join-Path $ReleaseDir $checksumAsset) -Destination $downloadDir
    } else {
        gh release download $Version --repo $Repository --pattern $asset --pattern $checksumAsset --dir $downloadDir
    }
    $archive = Join-Path $downloadDir $asset
    $checksum = Join-Path $downloadDir $checksumAsset
    if (-not $SkipAttestation) {
        if (-not (Get-Command gh -ErrorAction SilentlyContinue)) { throw "GitHub CLI is required for signed artifact verification." }
        gh attestation verify $archive --repo $Repository | Out-Null
    } elseif (-not $ReleaseDir) {
        throw "--SkipAttestation is allowed only with an explicit local --ReleaseDir."
    }

    $expected = ((Get-Content -LiteralPath $checksum -Raw).Trim() -split '\s+')[0].ToLowerInvariant()
    $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($expected -ne $actual) { throw "Release checksum verification failed." }
    Expand-Archive -LiteralPath $archive -DestinationPath $extractDir
    $source = Get-ChildItem -LiteralPath $extractDir -Directory | Select-Object -First 1
    if (-not $source) { throw "The integrated Hive archive has no distribution directory." }
    Test-HiveDistribution -Root $source.FullName -ExpectedOS "windows" -ExpectedArchitecture "amd64"

    if (Test-Path -LiteralPath $stagingDir) { Remove-SafeTree -Path $stagingDir -Parent ([IO.Path]::GetDirectoryName($InstallDir)) }
    Copy-Item -LiteralPath $source.FullName -Destination $stagingDir -Recurse
    Test-HiveDistribution -Root $stagingDir -ExpectedOS "windows" -ExpectedArchitecture "amd64"

    if (Test-Path -LiteralPath $backupDir) { Remove-SafeTree -Path $backupDir -Parent ([IO.Path]::GetDirectoryName($InstallDir)) }
    if (Test-Path -LiteralPath $InstallDir) { Move-Item -LiteralPath $InstallDir -Destination $backupDir }
    Move-Item -LiteralPath $stagingDir -Destination $InstallDir
    if (Test-Path -LiteralPath $backupDir) { Remove-SafeTree -Path $backupDir -Parent ([IO.Path]::GetDirectoryName($InstallDir)) }

    if (-not $NoPath) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $entries = @($userPath -split ';' | Where-Object { $_ })
        if (-not ($entries | Where-Object { [string]::Equals($_.TrimEnd('\'), $InstallDir.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase) })) {
            [Environment]::SetEnvironmentVariable("Path", (($entries + $InstallDir) -join ';'), "User")
        }
        if (-not (($env:Path -split ';') -contains $InstallDir)) { $env:Path = "$InstallDir;$env:Path" }
    }
    if (-not $NoCodexSkill) {
        $codexHome = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $HOME ".codex" }
        $skillsDir = Join-Path $codexHome "skills"
        $skillTarget = Join-Path $skillsDir "hive"
        New-Item -ItemType Directory -Path $skillsDir -Force | Out-Null
        if (Test-Path -LiteralPath $skillTarget) { Remove-SafeTree -Path $skillTarget -Parent $skillsDir }
        Copy-Item -LiteralPath (Join-Path $InstallDir "skills\hive") -Destination $skillTarget -Recurse
    }
    & (Join-Path $InstallDir "runtime\node.exe") (Join-Path $InstallDir "visual-hive\visual-hive.mjs") --version | Out-Null
    Write-Host "Hive $Version installed at $InstallDir"
    Write-Host "Next: gh auth login, then hive setup --repo owner/repository"
} finally {
    if (Test-Path -LiteralPath $tempRoot) { Remove-SafeTree -Path $tempRoot -Parent ([IO.Path]::GetTempPath()) }
    if (Test-Path -LiteralPath $stagingDir) { Remove-SafeTree -Path $stagingDir -Parent ([IO.Path]::GetDirectoryName($InstallDir)) }
}
}

function Test-HiveDistribution {
    param([Parameter(Mandatory)][string]$Root, [string]$ExpectedOS, [string]$ExpectedArchitecture)
    $resolvedRoot = [IO.Path]::GetFullPath($Root)
    $manifestPath = Join-Path $resolvedRoot "distribution-manifest.json"
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    if ($manifest.schema_version -ne "hive.integrated-distribution.v1" -or $manifest.os -ne $ExpectedOS -or $manifest.architecture -ne $ExpectedArchitecture) {
        throw "The integrated Hive distribution manifest does not match this platform."
    }
    foreach ($file in $manifest.files) {
        $relative = [string]$file.path
        if (-not $relative -or [IO.Path]::IsPathRooted($relative) -or $relative.Contains("\") -or ($relative -split '/') -contains '..') {
            throw "Unsafe distribution inventory path: $relative"
        }
        $target = [IO.Path]::GetFullPath((Join-Path $resolvedRoot ($relative -replace '/', [IO.Path]::DirectorySeparatorChar)))
        if (-not $target.StartsWith($resolvedRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw "Distribution path escaped the install root." }
        $item = Get-Item -LiteralPath $target
        if ($item.PSIsContainer -or $item.Length -ne [int64]$file.size) { throw "Distribution inventory size mismatch: $relative" }
        $digest = (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($digest -ne ([string]$file.sha256).ToLowerInvariant()) { throw "Distribution inventory digest mismatch: $relative" }
    }
    foreach ($required in @("hive.exe", "runtime/node.exe", "visual-hive/visual-hive.mjs", "visual-hive/release-manifest.json", "skills/hive/SKILL.md", "skills/hive/agents/openai.yaml")) {
        if (-not (Test-Path -LiteralPath (Join-Path $resolvedRoot ($required -replace '/', [IO.Path]::DirectorySeparatorChar)))) { throw "Distribution is missing $required" }
    }
}

function Remove-SafeTree {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Parent)
    $resolvedPath = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $resolvedParent = [IO.Path]::GetFullPath($Parent).TrimEnd('\')
    if (-not $resolvedPath.StartsWith($resolvedParent + '\', [StringComparison]::OrdinalIgnoreCase) -or $resolvedPath -eq $resolvedParent) {
        throw "Refusing to remove path outside the intended parent: $resolvedPath"
    }
    Remove-Item -LiteralPath $resolvedPath -Recurse -Force
}

Install-Hive
