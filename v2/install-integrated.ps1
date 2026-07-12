[CmdletBinding()]
param(
    [string]$Version = "latest",
    [string]$Repository = "DavidDiaz0317/hive",
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "Hive"),
    [string]$ReleaseDir = "",
    [switch]$SkipAttestation,
    [switch]$NoPath,
    [switch]$NoCodexSkill,
    [switch]$FunctionsOnly
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
if ($Repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') {
    throw "Repository must be an owner/name pair containing only letters, numbers, dots, underscores, or hyphens."
}
if (-not $ReleaseDir) {
    Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @("auth", "status") -Operation "GitHub CLI authentication check" | Out-Null
}
if ($Version -eq "latest") {
    if ($ReleaseDir) { throw "A concrete --Version is required with --ReleaseDir." }
    $versionOutput = @(Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @("api", "repos/$Repository/releases/latest", "--jq", ".tag_name") -Operation "GitHub latest release lookup")
    $Version = [string]::Join("`n", $versionOutput).Trim()
    if (-not $Version) { throw "No published integrated Hive release was found." }
}
if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$') {
    throw "Release version contains unsafe characters."
}
if (-not $ReleaseDir -and $Version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$') {
    throw "Published integrated release versions must match vMAJOR.MINOR.PATCH-integrated.NUMBER."
}
$releaseCommit = ""
if (-not $ReleaseDir) {
    $releaseCommit = Resolve-HiveReleaseCommit -Repository $Repository -Version $Version
}

$asset = "hive-integrated-$Version-windows-amd64.zip"
$checksumAsset = "$asset.sha256"
$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("hive-install-" + [guid]::NewGuid().ToString("N"))
$downloadDir = Join-Path $tempRoot "download"
$extractDir = Join-Path $tempRoot "extract"
$stagingDir = "$InstallDir.new-$([guid]::NewGuid().ToString('N'))"
$backupDir = "$InstallDir.previous"
$hadPreviousInstall = Test-Path -LiteralPath $InstallDir
$activated = $false
$committed = $false
$originalUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$originalProcessPath = $env:Path
$pathChanged = $false
$skillTarget = ""
$skillBackup = ""
$skillHadPrevious = $false
$skillBackupCompleted = $false
$skillCopyStarted = $false

try {
    New-Item -ItemType Directory -Path $downloadDir, $extractDir -Force | Out-Null
    if ($ReleaseDir) {
        Copy-Item -LiteralPath (Join-Path $ReleaseDir $asset) -Destination $downloadDir
        Copy-Item -LiteralPath (Join-Path $ReleaseDir $checksumAsset) -Destination $downloadDir
    } else {
        Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @("release", "download", $Version, "--repo", $Repository, "--pattern", $asset, "--pattern", $checksumAsset, "--dir", $downloadDir) -Operation "GitHub release download" | Out-Null
    }
    $archive = Join-Path $downloadDir $asset
    $checksum = Join-Path $downloadDir $checksumAsset
    if (-not $SkipAttestation) {
        if (-not (Get-Command gh -ErrorAction SilentlyContinue)) { throw "GitHub CLI is required for signed artifact verification." }
        Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @(
            "attestation", "verify", $archive,
            "--repo", $Repository,
            "--signer-workflow", "$Repository/.github/workflows/integrated-release.yml",
            "--source-ref", "refs/tags/$Version",
            "--source-digest", $releaseCommit,
            "--signer-digest", $releaseCommit,
            "--deny-self-hosted-runners"
        ) -Operation "GitHub artifact attestation verification" | Out-Null
        $currentReleaseCommit = Resolve-HiveReleaseCommit -Repository $Repository -Version $Version
        if ($currentReleaseCommit -ne $releaseCommit) {
            throw "Release tag $Version moved from $releaseCommit to $currentReleaseCommit during verification."
        }
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
    Invoke-HiveNativeCommand -FilePath (Join-Path $stagingDir "runtime\node.exe") -ArgumentList @((Join-Path $stagingDir "visual-hive\visual-hive.mjs"), "--version") -Operation "Bundled Visual Hive runtime check" | Out-Null

    Activate-HiveDistribution -StagingDir $stagingDir -InstallDir $InstallDir -BackupDir $backupDir
    $activated = $true

    if (-not $NoPath) {
        $userPath = $originalUserPath
        $entries = @($userPath -split ';' | Where-Object { $_ })
        if (-not ($entries | Where-Object { [string]::Equals($_.TrimEnd('\'), $InstallDir.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase) })) {
            [Environment]::SetEnvironmentVariable("Path", (($entries + $InstallDir) -join ';'), "User")
            $pathChanged = $true
        }
        if (-not (($env:Path -split ';') -contains $InstallDir)) { $env:Path = "$InstallDir;$env:Path" }
    }
    if (-not $NoCodexSkill) {
        $codexHome = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $HOME ".codex" }
        $skillsDir = Join-Path $codexHome "skills"
        $skillTarget = Join-Path $skillsDir "hive"
        $skillBackup = "$skillTarget.previous-$([guid]::NewGuid().ToString('N'))"
        New-Item -ItemType Directory -Path $skillsDir -Force | Out-Null
        if (Test-Path -LiteralPath $skillTarget) {
            Move-Item -LiteralPath $skillTarget -Destination $skillBackup
            $skillHadPrevious = $true
            $skillBackupCompleted = $true
        }
        $skillCopyStarted = $true
        Copy-Item -LiteralPath (Join-Path $InstallDir "skills\hive") -Destination $skillTarget -Recurse
    }
    $committed = $true
    # Backups are retained through the complete install/PATH/skill transaction.
    # Cleanup after the commit point is best effort; a leftover backup is safer
    # than rolling back a working install because cleanup itself was denied.
    if ($skillBackup -and (Test-Path -LiteralPath $skillBackup)) {
        try { Remove-SafeTree -Path $skillBackup -Parent ([IO.Path]::GetDirectoryName($skillTarget)) } catch { Write-Warning "Hive installed, but the prior Codex skill backup remains at $skillBackup" }
    }
    if (Test-Path -LiteralPath $backupDir) {
        try { Remove-SafeTree -Path $backupDir -Parent ([IO.Path]::GetDirectoryName($InstallDir)) } catch { Write-Warning "Hive installed, but the prior installation backup remains at $backupDir" }
    }
    Write-Host "Hive $Version installed at $InstallDir"
    Write-Host "Next (after 'gh auth login' only if needed): hive setup --repo owner/repository --coverage comprehensive --automation auto-merge --provider codex --visual-hive --start --json"
} catch {
    $installError = $_.Exception.Message
    if ($activated -and -not $committed) {
        $rollbackErrors = @()
        try {
            if ($pathChanged) { [Environment]::SetEnvironmentVariable("Path", $originalUserPath, "User") }
            $env:Path = $originalProcessPath
        } catch { $rollbackErrors += "PATH restore: $($_.Exception.Message)" }
        try {
            # Do not touch a pre-existing skill when its backup Move-Item failed.
            # Once the backup completed (or a copy began with no prior skill),
            # removing the candidate is safe and required for rollback.
            if (($skillBackupCompleted -or $skillCopyStarted) -and $skillTarget -and (Test-Path -LiteralPath $skillTarget)) {
                Remove-SafeTree -Path $skillTarget -Parent ([IO.Path]::GetDirectoryName($skillTarget))
            }
            if ($skillBackupCompleted -and $skillHadPrevious -and (Test-Path -LiteralPath $skillBackup)) {
                Move-Item -LiteralPath $skillBackup -Destination $skillTarget
            }
        } catch { $rollbackErrors += "Codex skill restore: $($_.Exception.Message)" }
        try {
            if (Test-Path -LiteralPath $InstallDir) { Remove-SafeTree -Path $InstallDir -Parent ([IO.Path]::GetDirectoryName($InstallDir)) }
            if ($hadPreviousInstall -and (Test-Path -LiteralPath $backupDir)) {
                Move-Item -LiteralPath $backupDir -Destination $InstallDir
            }
        } catch { $rollbackErrors += "installation restore: $($_.Exception.Message)" }
        if ($rollbackErrors.Count -gt 0) {
            throw "Hive installation failed ($installError) and rollback was incomplete: $($rollbackErrors -join '; ')"
        }
        throw "Hive installation failed ($installError); the previous installation, PATH, and Codex skill were restored."
    }
    throw
} finally {
    if (Test-Path -LiteralPath $tempRoot) { Remove-SafeTree -Path $tempRoot -Parent ([IO.Path]::GetTempPath()) }
    if (Test-Path -LiteralPath $stagingDir) { Remove-SafeTree -Path $stagingDir -Parent ([IO.Path]::GetDirectoryName($InstallDir)) }
}
}

function Invoke-HiveNativeCommand {
    param(
        [Parameter(Mandatory)][string]$FilePath,
        [string[]]$ArgumentList = @(),
        [Parameter(Mandatory)][string]$Operation
    )
    $output = & $FilePath @ArgumentList
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        throw "$Operation failed with exit code $exitCode."
    }
    return $output
}

function Resolve-HiveReleaseCommit {
    param(
        [Parameter(Mandatory)][string]$Repository,
        [Parameter(Mandatory)][string]$Version
    )
    $output = @(Invoke-HiveNativeCommand -FilePath "gh" -ArgumentList @("api", "repos/$Repository/commits/$Version", "--jq", ".sha") -Operation "GitHub release tag resolution")
    $commit = [string]::Join("`n", $output).Trim().ToLowerInvariant()
    if ($commit -notmatch '^[a-f0-9]{40}$') {
        throw "Release tag $Version did not resolve to an exact 40-character commit."
    }
    return $commit
}

function Activate-HiveDistribution {
    param(
        [Parameter(Mandatory)][string]$StagingDir,
        [Parameter(Mandatory)][string]$InstallDir,
        [Parameter(Mandatory)][string]$BackupDir
    )
    $parent = [IO.Path]::GetDirectoryName($InstallDir)
    $hadPrevious = Test-Path -LiteralPath $InstallDir
    if (Test-Path -LiteralPath $BackupDir) { Remove-SafeTree -Path $BackupDir -Parent $parent }
    if ($hadPrevious) { Move-Item -LiteralPath $InstallDir -Destination $BackupDir }
    try {
        Move-Item -LiteralPath $StagingDir -Destination $InstallDir
    } catch {
        $activationError = $_.Exception.Message
        if ($hadPrevious -and (Test-Path -LiteralPath $BackupDir)) {
            try {
                if (Test-Path -LiteralPath $InstallDir) { Remove-SafeTree -Path $InstallDir -Parent $parent }
                Move-Item -LiteralPath $BackupDir -Destination $InstallDir
            } catch {
                $restoreError = $_.Exception.Message
                throw "Hive activation failed ($activationError) and the previous installation could not be restored ($restoreError). The backup remains at $BackupDir."
            }
            throw "Hive activation failed ($activationError); the previous installation was restored."
        }
        throw "Hive activation failed: $activationError"
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
    # The archive inventory must follow Windows filesystem identity. Paths that
    # differ only by case name the same file and therefore constitute a duplicate.
    $inventoried = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
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
        if (-not $inventoried.Add($relative)) { throw "Duplicate distribution inventory path: $relative" }
    }
    # Windows PowerShell 5.1 runs on .NET Framework, which does not expose
    # Path.GetRelativePath. The inventory is rooted above, so a checked prefix
    # trim is both compatible and fail-closed.
    $rootPrefix = $resolvedRoot.TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    $actual = Get-ChildItem -LiteralPath $resolvedRoot -Recurse -Force -File | ForEach-Object {
        $fullName = [IO.Path]::GetFullPath($_.FullName)
        if (-not $fullName.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw "Distribution inventory path escaped the install root: $fullName"
        }
        $fullName.Substring($rootPrefix.Length).Replace('\', '/')
    }
    $expectedActual = [Collections.Generic.HashSet[string]]::new($inventoried, [StringComparer]::OrdinalIgnoreCase)
    [void]$expectedActual.Add("distribution-manifest.json")
    foreach ($relative in $actual) {
        if (-not $expectedActual.Contains($relative)) { throw "Uninventoried distribution file: $relative" }
    }
    foreach ($relative in $expectedActual) {
        if ($actual -notcontains $relative) { throw "Missing distribution file: $relative" }
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

if (-not $FunctionsOnly) {
    Install-Hive
}
