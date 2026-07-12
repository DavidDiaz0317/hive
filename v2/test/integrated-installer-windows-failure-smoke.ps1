[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Installer,
    [Parameter(Mandatory)][string]$ReleaseDir,
    [Parameter(Mandatory)][string]$Version,
    [Parameter(Mandatory)][string]$WorkRoot
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$resolvedWorkRoot = [IO.Path]::GetFullPath($WorkRoot).TrimEnd('\')
$resolvedReleaseDir = [IO.Path]::GetFullPath($ReleaseDir).TrimEnd('\')
$resolvedInstaller = [IO.Path]::GetFullPath($Installer)
$assetName = "hive-integrated-$Version-windows-amd64.zip"
$checksumName = "$assetName.sha256"
$sourceAsset = Join-Path $resolvedReleaseDir $assetName
$sourceChecksum = Join-Path $resolvedReleaseDir $checksumName
foreach ($required in @($resolvedInstaller, $sourceAsset, $sourceChecksum)) {
    if (-not (Test-Path -LiteralPath $required -PathType Leaf)) { throw "Required installer failure-smoke input is missing: $required" }
}
if (Test-Path -LiteralPath $resolvedWorkRoot) {
    $parent = [IO.Path]::GetDirectoryName($resolvedWorkRoot)
    if (-not $parent -or $resolvedWorkRoot -eq [IO.Path]::GetPathRoot($resolvedWorkRoot)) { throw "Unsafe failure-smoke work root: $resolvedWorkRoot" }
    Remove-Item -LiteralPath $resolvedWorkRoot -Recurse -Force
}
New-Item -ItemType Directory -Path $resolvedWorkRoot -Force | Out-Null

$fakeBin = Join-Path $resolvedWorkRoot "fake-bin"
New-Item -ItemType Directory -Path $fakeBin -Force | Out-Null
$fakeGhScript = @'
$ErrorActionPreference = "Stop"
$mode = [string]$env:HIVE_FAKE_GH_MODE
$command = if ($args.Count -gt 0) { [string]$args[0] } else { "" }
if ($command -eq "auth" -and $args.Count -gt 1 -and $args[1] -eq "status") {
    if ($mode -eq "auth-fail") { exit 40 }
    exit 0
}
if ($command -eq "api") {
    if ($mode -eq "api-fail") { exit 41 }
    $endpoint = if ($args.Count -gt 1) { [string]$args[1] } else { "" }
    if ($endpoint -like "repos/*/releases/latest") {
        Write-Output $env:HIVE_FAKE_VERSION
        exit 0
    }
    if ($endpoint -like "repos/*/commits/*") {
        if ($mode -eq "commit-invalid") { Write-Output "not-a-commit"; exit 0 }
        if ($mode -eq "tag-move") {
            if (Test-Path -LiteralPath $env:HIVE_FAKE_TAG_COUNTER) {
                Write-Output $env:HIVE_FAKE_MOVED_COMMIT
            } else {
                New-Item -ItemType File -Path $env:HIVE_FAKE_TAG_COUNTER | Out-Null
                Write-Output $env:HIVE_FAKE_COMMIT
            }
            exit 0
        }
        Write-Output $env:HIVE_FAKE_COMMIT
        exit 0
    }
    exit 50
}
if ($command -eq "release" -and $args.Count -gt 1 -and $args[1] -eq "download") {
    if ($mode -eq "download-fail") { exit 42 }
    $dirIndex = [Array]::IndexOf([object[]]$args, "--dir")
    if ($dirIndex -lt 0 -or $dirIndex + 1 -ge $args.Count) { exit 45 }
    $destination = [string]$args[$dirIndex + 1]
    $patterns = @()
    for ($index = 0; $index -lt $args.Count - 1; $index += 1) {
        if ($args[$index] -eq "--pattern") { $patterns += [string]$args[$index + 1] }
    }
    if ($patterns.Count -ne 2) { exit 46 }
    Copy-Item -LiteralPath (Join-Path $env:HIVE_FAKE_RELEASE_DIR $env:HIVE_FAKE_ASSET) -Destination (Join-Path $destination $patterns[0])
    Copy-Item -LiteralPath (Join-Path $env:HIVE_FAKE_RELEASE_DIR ($env:HIVE_FAKE_ASSET + ".sha256")) -Destination (Join-Path $destination $patterns[1])
    exit 0
}
if ($command -eq "attestation") {
    $signerIndex = [Array]::IndexOf([object[]]$args, "--signer-workflow")
    if ($signerIndex -lt 0 -or $signerIndex + 1 -ge $args.Count) { exit 47 }
    if ([string]$args[$signerIndex + 1] -ne "DavidDiaz0317/hive/.github/workflows/integrated-release.yml") { exit 48 }
    if (-not ([object[]]$args -contains "--deny-self-hosted-runners")) { exit 49 }
    $sourceRefIndex = [Array]::IndexOf([object[]]$args, "--source-ref")
    if ($sourceRefIndex -lt 0 -or [string]$args[$sourceRefIndex + 1] -ne "refs/tags/$($env:HIVE_FAKE_PUBLIC_VERSION)") { exit 51 }
    foreach ($flag in @("--source-digest", "--signer-digest")) {
        $digestIndex = [Array]::IndexOf([object[]]$args, $flag)
        if ($digestIndex -lt 0 -or [string]$args[$digestIndex + 1] -ne $env:HIVE_FAKE_COMMIT) { exit 52 }
    }
    if ($mode -eq "attestation-fail") { exit 43 }
    exit 0
}
exit 44
'@
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[IO.File]::WriteAllText((Join-Path $fakeBin "fake-gh.ps1"), $fakeGhScript, $utf8NoBom)
[IO.File]::WriteAllText(
    (Join-Path $fakeBin "gh.cmd"),
    "@echo off`r`npowershell.exe -NoProfile -ExecutionPolicy Bypass -File `"%~dp0fake-gh.ps1`" %*`r`nexit /b %ERRORLEVEL%`r`n",
    [Text.Encoding]::ASCII
)

$powershell = (Get-Command powershell.exe -ErrorAction Stop).Source
$originalPath = $env:Path
$originalMode = $env:HIVE_FAKE_GH_MODE
$originalReleaseDir = $env:HIVE_FAKE_RELEASE_DIR
$originalAsset = $env:HIVE_FAKE_ASSET
$originalVersion = $env:HIVE_FAKE_VERSION
$originalCommit = $env:HIVE_FAKE_COMMIT
$originalPublicVersion = $env:HIVE_FAKE_PUBLIC_VERSION
$originalMovedCommit = $env:HIVE_FAKE_MOVED_COMMIT
$originalTagCounter = $env:HIVE_FAKE_TAG_COUNTER
$originalCodexHome = $env:CODEX_HOME

function Invoke-InstallerProcess {
    param(
        [Parameter(Mandatory)][string]$Name,
        [string]$RequestedVersion,
        [string]$LocalReleaseDir,
        [switch]$SkipAttestation,
        [switch]$WithCodexSkill
    )
    $installDir = Join-Path $resolvedWorkRoot "install-$Name"
    $arguments = @(
        "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $resolvedInstaller,
        "-Repository", "DavidDiaz0317/hive",
        "-InstallDir", $installDir,
        "-NoPath"
    )
	if (-not $WithCodexSkill) { $arguments += "-NoCodexSkill" }
    if ($RequestedVersion) { $arguments += @("-Version", $RequestedVersion) }
    if ($LocalReleaseDir) { $arguments += @("-ReleaseDir", $LocalReleaseDir) }
    if ($SkipAttestation) { $arguments += "-SkipAttestation" }
    $previousErrorPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = "Continue"
        $global:LASTEXITCODE = 0
        $output = (& $powershell @arguments 2>&1 | Out-String)
        $exitCode = $global:LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorPreference
    }
    return [pscustomobject]@{ Name = $Name; InstallDir = $installDir; ExitCode = $exitCode; Output = $output }
}

function Assert-InstallerFailure {
    param(
        [Parameter(Mandatory)]$Result,
        [Parameter(Mandatory)][string]$ExpectedMessage
    )
    if ($Result.ExitCode -eq 0) { throw "$($Result.Name) unexpectedly succeeded.`n$($Result.Output)" }
    if ($Result.Output -notmatch [regex]::Escape($ExpectedMessage)) {
        throw "$($Result.Name) did not report the expected failure '$ExpectedMessage'.`n$($Result.Output)"
    }
}

function New-NodeFailureRelease {
    $nodeFailureRoot = Join-Path $resolvedWorkRoot "node-failure-release"
    $extractRoot = Join-Path $resolvedWorkRoot "node-failure-extract"
    New-Item -ItemType Directory -Path $nodeFailureRoot, $extractRoot -Force | Out-Null
    Expand-Archive -LiteralPath $sourceAsset -DestinationPath $extractRoot
    $roots = @(Get-ChildItem -LiteralPath $extractRoot -Directory)
    if ($roots.Count -ne 1) { throw "Expected one archive root while preparing the Node failure fixture." }
    $distributionRoot = $roots[0].FullName
    $nodePath = Join-Path $distributionRoot "runtime\node.exe"
    Copy-Item -LiteralPath (Join-Path $distributionRoot "hive.exe") -Destination $nodePath -Force
    $nodeInfo = Get-Item -LiteralPath $nodePath
    $nodeHash = (Get-FileHash -LiteralPath $nodePath -Algorithm SHA256).Hash.ToLowerInvariant()
    $manifestPath = Join-Path $distributionRoot "distribution-manifest.json"
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $nodeRows = @($manifest.files | Where-Object { $_.path -eq "runtime/node.exe" })
    if ($nodeRows.Count -ne 1) { throw "Node runtime inventory row was not found." }
    $nodeRows[0].size = [int64]$nodeInfo.Length
    $nodeRows[0].sha256 = $nodeHash
    [IO.File]::WriteAllText($manifestPath, (($manifest | ConvertTo-Json -Depth 100) + "`n"), $utf8NoBom)
    $asset = Join-Path $nodeFailureRoot $assetName
    Compress-Archive -LiteralPath $distributionRoot -DestinationPath $asset -CompressionLevel Optimal
    $hash = (Get-FileHash -LiteralPath $asset -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $nodeFailureRoot $checksumName), "$hash  $assetName`r`n", [Text.Encoding]::ASCII)
    return $nodeFailureRoot
}

function New-ExtraFileRelease {
    $extraRoot = Join-Path $resolvedWorkRoot "extra-file-release"
    $extractRoot = Join-Path $resolvedWorkRoot "extra-file-extract"
    New-Item -ItemType Directory -Path $extraRoot, $extractRoot -Force | Out-Null
    Expand-Archive -LiteralPath $sourceAsset -DestinationPath $extractRoot
    $roots = @(Get-ChildItem -LiteralPath $extractRoot -Directory)
    if ($roots.Count -ne 1) { throw "Expected one archive root while preparing the extra-file fixture." }
    [IO.File]::WriteAllText((Join-Path $roots[0].FullName "uninventoried.txt"), "must be rejected", $utf8NoBom)
    $asset = Join-Path $extraRoot $assetName
    Compress-Archive -LiteralPath $roots[0].FullName -DestinationPath $asset -CompressionLevel Optimal
    $hash = (Get-FileHash -LiteralPath $asset -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $extraRoot $checksumName), "$hash  $assetName`r`n", [Text.Encoding]::ASCII)
    return $extraRoot
}

function New-CaseDuplicateRelease {
    $duplicateRoot = Join-Path $resolvedWorkRoot "case-duplicate-release"
    $extractRoot = Join-Path $resolvedWorkRoot "case-duplicate-extract"
    New-Item -ItemType Directory -Path $duplicateRoot, $extractRoot -Force | Out-Null
    Expand-Archive -LiteralPath $sourceAsset -DestinationPath $extractRoot
    $roots = @(Get-ChildItem -LiteralPath $extractRoot -Directory)
    if ($roots.Count -ne 1) { throw "Expected one archive root while preparing the case-duplicate fixture." }
    $manifestPath = Join-Path $roots[0].FullName "distribution-manifest.json"
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    $original = @($manifest.files)[0]
    $duplicate = [pscustomobject]@{
        path = ([string]$original.path).ToUpperInvariant()
        size = [int64]$original.size
        sha256 = [string]$original.sha256
    }
    $manifest.files = @($manifest.files) + @($duplicate)
    [IO.File]::WriteAllText($manifestPath, (($manifest | ConvertTo-Json -Depth 100) + "`n"), $utf8NoBom)
    $asset = Join-Path $duplicateRoot $assetName
    Compress-Archive -LiteralPath $roots[0].FullName -DestinationPath $asset -CompressionLevel Optimal
    $hash = (Get-FileHash -LiteralPath $asset -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $duplicateRoot $checksumName), "$hash  $assetName`r`n", [Text.Encoding]::ASCII)
    return $duplicateRoot
}

try {
    $env:Path = "$fakeBin;$originalPath"
    $env:HIVE_FAKE_RELEASE_DIR = $resolvedReleaseDir
    $env:HIVE_FAKE_ASSET = $assetName
    $env:HIVE_FAKE_VERSION = $Version
    $env:HIVE_FAKE_COMMIT = "0123456789abcdef0123456789abcdef01234567"
    $publishedTestVersion = "v0.0.0-integrated.999999"
    $env:HIVE_FAKE_PUBLIC_VERSION = $publishedTestVersion
    $env:HIVE_FAKE_MOVED_COMMIT = "89abcdef0123456789abcdef0123456789abcdef"
    $env:HIVE_FAKE_TAG_COUNTER = Join-Path $resolvedWorkRoot "fake-tag-counter"

    $env:HIVE_FAKE_GH_MODE = "auth-fail"
    Assert-InstallerFailure (Invoke-InstallerProcess -Name "auth-fail") "GitHub CLI authentication check failed with exit code 40."

    $env:HIVE_FAKE_GH_MODE = "api-fail"
    Assert-InstallerFailure (Invoke-InstallerProcess -Name "api-fail") "GitHub latest release lookup failed with exit code 41."

    $env:HIVE_FAKE_GH_MODE = "download-fail"
    Assert-InstallerFailure (Invoke-InstallerProcess -Name "download-fail" -RequestedVersion $publishedTestVersion) "GitHub release download failed with exit code 42."

    $env:HIVE_FAKE_GH_MODE = "attestation-fail"
    Assert-InstallerFailure (Invoke-InstallerProcess -Name "attestation-fail" -RequestedVersion $publishedTestVersion) "GitHub artifact attestation verification failed with exit code 43."

    $env:HIVE_FAKE_GH_MODE = "commit-invalid"
    Assert-InstallerFailure (Invoke-InstallerProcess -Name "commit-invalid" -RequestedVersion $publishedTestVersion) "did not resolve to an exact 40-character commit"

    Remove-Item -LiteralPath $env:HIVE_FAKE_TAG_COUNTER -Force -ErrorAction SilentlyContinue
    $env:HIVE_FAKE_GH_MODE = "tag-move"
    Assert-InstallerFailure (Invoke-InstallerProcess -Name "tag-move" -RequestedVersion $publishedTestVersion) "moved from"

    Assert-InstallerFailure (Invoke-InstallerProcess -Name "unsafe-version" -RequestedVersion "../unsafe" -LocalReleaseDir $resolvedReleaseDir -SkipAttestation) "Release version contains unsafe characters."
    Assert-InstallerFailure (Invoke-InstallerProcess -Name "invalid-published-version" -RequestedVersion "v1-dev" ) "Published integrated release versions must match vMAJOR.MINOR.PATCH-integrated.NUMBER."

    $extraFileRelease = New-ExtraFileRelease
    $extraFileFailure = Invoke-InstallerProcess -Name "extra-file-fail" -RequestedVersion $Version -LocalReleaseDir $extraFileRelease -SkipAttestation
    Assert-InstallerFailure $extraFileFailure "Uninventoried distribution file: uninventoried.txt"

    $caseDuplicateRelease = New-CaseDuplicateRelease
    $caseDuplicateFailure = Invoke-InstallerProcess -Name "case-duplicate-fail" -RequestedVersion $Version -LocalReleaseDir $caseDuplicateRelease -SkipAttestation
    Assert-InstallerFailure $caseDuplicateFailure "Duplicate distribution inventory path:"

    $nodeFailureRelease = New-NodeFailureRelease
    $nodeInstall = Join-Path $resolvedWorkRoot "install-node-fail"
    New-Item -ItemType Directory -Path $nodeInstall -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $nodeInstall "previous.txt"), "previous installation", $utf8NoBom)
    $nodeFailure = Invoke-InstallerProcess -Name "node-fail" -RequestedVersion $Version -LocalReleaseDir $nodeFailureRelease -SkipAttestation
    Assert-InstallerFailure $nodeFailure "Bundled Visual Hive runtime check failed with exit code"
    if (-not (Test-Path -LiteralPath (Join-Path $nodeInstall "previous.txt") -PathType Leaf)) {
        throw "The failed bundled Node smoke replaced the previous installation."
    }

    . $resolvedInstaller -Version $Version -FunctionsOnly
    $activationRoot = Join-Path $resolvedWorkRoot "activation-rollback"
    $activationInstall = Join-Path $activationRoot "installed"
    $activationStaging = Join-Path $activationRoot "staging"
    $activationBackup = Join-Path $activationRoot "installed.previous"
    New-Item -ItemType Directory -Path $activationInstall, $activationStaging -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $activationInstall "previous.txt"), "previous installation", $utf8NoBom)
    [IO.File]::WriteAllText((Join-Path $activationStaging "candidate.txt"), "candidate installation", $utf8NoBom)
    $global:HiveActivationMoveCount = 0
    function global:Move-Item {
        param([Parameter(Mandatory)][string]$LiteralPath, [Parameter(Mandatory)][string]$Destination)
        $global:HiveActivationMoveCount += 1
        if ($global:HiveActivationMoveCount -eq 2) { throw "synthetic activation failure" }
        Microsoft.PowerShell.Management\Move-Item -LiteralPath $LiteralPath -Destination $Destination -ErrorAction Stop
    }
    $activationFailure = ""
    try {
        Activate-HiveDistribution -StagingDir $activationStaging -InstallDir $activationInstall -BackupDir $activationBackup
    } catch {
        $activationFailure = $_.Exception.Message
    } finally {
        Remove-Item -LiteralPath Function:\Move-Item -Force
        Remove-Variable -Name HiveActivationMoveCount -Scope Global -ErrorAction SilentlyContinue
    }
    if ($activationFailure -notmatch "previous installation was restored") { throw "Activation rollback did not report a successful restore: $activationFailure" }
    if (-not (Test-Path -LiteralPath (Join-Path $activationInstall "previous.txt") -PathType Leaf)) { throw "Activation rollback did not restore the prior installation." }
    if (Test-Path -LiteralPath (Join-Path $activationInstall "candidate.txt")) { throw "Activation rollback left candidate bytes active." }
    if (Test-Path -LiteralPath $activationBackup) { throw "Activation rollback left the restored backup behind." }

    # A failed attempt to move the pre-existing Codex skill to its backup must
    # leave those original bytes in place. The rollback must not assume that a
    # backup exists and delete the source it failed to move.
    $skillMoveInstall = Join-Path $resolvedWorkRoot "install-skill-backup-move-fail"
    $skillMoveCodexHome = Join-Path $resolvedWorkRoot "codex-skill-backup-move-fail"
    $skillMoveTarget = Join-Path $skillMoveCodexHome "skills\hive"
    New-Item -ItemType Directory -Path $skillMoveInstall, $skillMoveTarget -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $skillMoveInstall "previous.txt"), "previous installation", $utf8NoBom)
    [IO.File]::WriteAllText((Join-Path $skillMoveTarget "original-skill.txt"), "original skill", $utf8NoBom)
    $env:CODEX_HOME = $skillMoveCodexHome
    . $resolvedInstaller -Version $Version -Repository "DavidDiaz0317/hive" -InstallDir $skillMoveInstall -ReleaseDir $resolvedReleaseDir -SkipAttestation -NoPath -FunctionsOnly
    $global:HiveSkillMoveFailTarget = [IO.Path]::GetFullPath($skillMoveTarget)
    function global:Move-Item {
        param([Parameter(Mandatory)][string]$LiteralPath, [Parameter(Mandatory)][string]$Destination)
        if ([IO.Path]::GetFullPath($LiteralPath) -eq $global:HiveSkillMoveFailTarget) {
            throw "synthetic skill backup move failure"
        }
        Microsoft.PowerShell.Management\Move-Item -LiteralPath $LiteralPath -Destination $Destination -ErrorAction Stop
    }
    $skillMoveFailure = ""
    try {
        Install-Hive
    } catch {
        $skillMoveFailure = $_.Exception.Message
    } finally {
        Remove-Item -LiteralPath Function:\Move-Item -Force
        Remove-Variable -Name HiveSkillMoveFailTarget -Scope Global -ErrorAction SilentlyContinue
    }
    if ($skillMoveFailure -notmatch "synthetic skill backup move failure") { throw "Expected the synthetic skill backup failure: $skillMoveFailure" }
    if (-not (Test-Path -LiteralPath (Join-Path $skillMoveTarget "original-skill.txt") -PathType Leaf)) {
        throw "A failed skill backup move deleted the pre-existing Codex skill."
    }
    if (-not (Test-Path -LiteralPath (Join-Path $skillMoveInstall "previous.txt") -PathType Leaf)) {
        throw "A failed skill backup move did not restore the previous Hive installation."
    }

	$postActivationInstall = Join-Path $resolvedWorkRoot "install-post-activation-skill-fail"
	New-Item -ItemType Directory -Path $postActivationInstall -Force | Out-Null
	[IO.File]::WriteAllText((Join-Path $postActivationInstall "previous.txt"), "previous installation", $utf8NoBom)
	$blockedCodexHome = Join-Path $resolvedWorkRoot "blocked-codex-home"
	New-Item -ItemType Directory -Path $blockedCodexHome -Force | Out-Null
	[IO.File]::WriteAllText((Join-Path $blockedCodexHome "skills"), "blocks directory creation", $utf8NoBom)
	$env:CODEX_HOME = $blockedCodexHome
	$postActivationFailure = Invoke-InstallerProcess -Name "post-activation-skill-fail" -RequestedVersion $Version -LocalReleaseDir $resolvedReleaseDir -SkipAttestation -WithCodexSkill
	Assert-InstallerFailure $postActivationFailure "the previous installation, PATH, and Codex skill were restored"
	if (-not (Test-Path -LiteralPath (Join-Path $postActivationInstall "previous.txt") -PathType Leaf)) {
		throw "A post-activation Codex skill failure did not restore the previous installation."
	}
} finally {
    $env:Path = $originalPath
    $env:HIVE_FAKE_GH_MODE = $originalMode
    $env:HIVE_FAKE_RELEASE_DIR = $originalReleaseDir
    $env:HIVE_FAKE_ASSET = $originalAsset
    $env:HIVE_FAKE_VERSION = $originalVersion
	$env:HIVE_FAKE_COMMIT = $originalCommit
	$env:HIVE_FAKE_PUBLIC_VERSION = $originalPublicVersion
	$env:HIVE_FAKE_MOVED_COMMIT = $originalMovedCommit
	$env:HIVE_FAKE_TAG_COUNTER = $originalTagCounter
	$env:CODEX_HOME = $originalCodexHome
}

Write-Host "Windows installer failure smoke passed: $resolvedWorkRoot"
# Expected child-process failures are the substance of this smoke. PowerShell
# otherwise propagates the final child's nonzero LASTEXITCODE even after every
# assertion passes, which makes a successful hosted runner step fail.
$global:LASTEXITCODE = 0
