param(
    [Parameter(Mandatory)][string]$HiveRoot,
    [Parameter(Mandatory)][string]$VisualBundle,
    [Parameter(Mandatory)][string]$WorkRoot
)

$ErrorActionPreference = "Stop"
$hiveCommit = (git -C $HiveRoot rev-parse HEAD).Trim()
$visualManifest = Get-Content -LiteralPath (Join-Path $VisualBundle "release-manifest.json") -Raw | ConvertFrom-Json
$visualCommit = [string]$visualManifest.gitCommit
$node = (Get-Command node).Source
$nodeVersion = (& $node --version).Trim()
$nodeRoot = Split-Path -Parent $node
$releaseVersion = "vlocal-windows"

New-Item -ItemType Directory -Path $WorkRoot -Force | Out-Null
$nodeRuntime = Join-Path $WorkRoot "node-runtime"
New-Item -ItemType Directory -Path (Join-Path $nodeRuntime "node_modules") -Force | Out-Null
Copy-Item -LiteralPath $node -Destination (Join-Path $nodeRuntime "node.exe")
foreach ($name in @("npm.cmd", "npx.cmd", "corepack.cmd")) {
    Copy-Item -LiteralPath (Join-Path $nodeRoot $name) -Destination $nodeRuntime
}
Copy-Item -LiteralPath (Join-Path $nodeRoot "node_modules/npm") -Destination (Join-Path $nodeRuntime "node_modules/npm") -Recurse
Copy-Item -LiteralPath (Join-Path $nodeRoot "node_modules/corepack") -Destination (Join-Path $nodeRuntime "node_modules/corepack") -Recurse
foreach ($name in @("pnpm.cmd", "pnpx.cmd", "yarn.cmd", "yarnpkg.cmd")) {
    Copy-Item -LiteralPath (Join-Path $HiveRoot "runtime-launchers/windows/$name") -Destination $nodeRuntime
}
$nodeLicense = Join-Path $nodeRoot "LICENSE"
if (Test-Path -LiteralPath $nodeLicense -PathType Leaf) {
    Copy-Item -LiteralPath $nodeLicense -Destination (Join-Path $nodeRuntime "LICENSE.node.txt")
}
$distribution = Join-Path $WorkRoot "hive-integrated-$releaseVersion-windows-amd64"
Push-Location $HiveRoot
try {
    go build -trimpath -ldflags "-s -w -X main.gitHash=$hiveCommit -X main.gitShort=$($hiveCommit.Substring(0,12))" -o (Join-Path $WorkRoot "hive.exe") ./cmd/hive
	$distributionArgs = @(
		"run", "./cmd/hive-dist", "--hive", (Join-Path $WorkRoot "hive.exe"), "--hive-commit", $hiveCommit,
		"--visual-hive", $VisualBundle, "--visual-hive-commit", $visualCommit, "--node", $node,
		"--node-runtime", $nodeRuntime, "--node-version", $nodeVersion, "--skill", (Join-Path $HiveRoot "skills/hive"),
		"--target-os", "windows", "--target-arch", "amd64", "--output", $distribution
	)
	if (Test-Path -LiteralPath $nodeLicense -PathType Leaf) { $distributionArgs += @("--node-license", $nodeLicense) }
	& go @distributionArgs | Out-Null
	if ($LASTEXITCODE -ne 0) { throw "Hive distribution build failed with exit code $LASTEXITCODE." }
} finally {
    Pop-Location
}
$asset = Join-Path $WorkRoot "hive-integrated-$releaseVersion-windows-amd64.zip"
Compress-Archive -LiteralPath $distribution -DestinationPath $asset -CompressionLevel Optimal
$hash = (Get-FileHash -LiteralPath $asset -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath "$asset.sha256" -Value "$hash  $([IO.Path]::GetFileName($asset))" -Encoding ascii

$installDir = Join-Path $WorkRoot "installed"
$earlierHiveDir = Join-Path $WorkRoot "earlier-hive"
New-Item -ItemType Directory -Path $earlierHiveDir -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $WorkRoot "hive.exe") -Destination (Join-Path $earlierHiveDir "hive.exe")
$originalProcessPath = $env:Path
$originalUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$originalCodexHome = $env:CODEX_HOME
try {
    # Prove the default installation wins over an older Hive already on PATH,
    # both in this process and in a fresh process that reads the User PATH.
	$env:Path = "$earlierHiveDir;$originalProcessPath"
	$env:CODEX_HOME = Join-Path $WorkRoot "codex-home"
	[Environment]::SetEnvironmentVariable("Path", "$earlierHiveDir;$originalUserPath", "User")
	$installOutput = (& (Join-Path $HiveRoot "install-integrated.ps1") -Version $releaseVersion -ReleaseDir $WorkRoot -SkipAttestation -InstallDir $installDir 6>&1 | Out-String)
    $resolvedHive = (@(Get-Command hive -CommandType Application -ErrorAction Stop)[0]).Source
    if (-not [string]::Equals([IO.Path]::GetFullPath($resolvedHive), [IO.Path]::GetFullPath((Join-Path $installDir "hive.exe")), [StringComparison]::OrdinalIgnoreCase)) {
        throw "Installed Hive did not take precedence over the older PATH entry: $resolvedHive"
    }
    $userPathFirst = @([Environment]::GetEnvironmentVariable("Path", "User") -split ';' | Where-Object { $_ })[0]
    if (-not [string]::Equals($userPathFirst.TrimEnd('\'), $installDir.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)) {
        throw "Installed Hive directory was not prepended to the persistent User PATH."
    }
    $exactNextCommand = "& `"$(Join-Path $installDir 'hive.exe')`" setup"
	if ($installOutput -notmatch [regex]::Escape($exactNextCommand)) {
        throw "Installer did not print the exact absolute setup command.`n$installOutput"
	}
	if (-not (Test-Path -LiteralPath (Join-Path $env:CODEX_HOME "skills/hive/SKILL.md") -PathType Leaf)) {
		throw "Installer default path did not install the packaged Hive Codex skill."
	}

    & (Join-Path $installDir "runtime/node.exe") (Join-Path $installDir "visual-hive/visual-hive.mjs") --version | Out-Null
    $plan = & (Join-Path $installDir "hive.exe") setup --repo DavidDiaz0317/visual-hive-demo-site --coverage comprehensive --automation advisory --provider codex --visual-hive --plan --state-dir (Join-Path $WorkRoot "state") --json | ConvertFrom-Json
    if ($plan.plan.schema_version -ne "hive.setup-plan.v1" -or -not $plan.plan.read_only) { throw "Installed Hive did not produce a read-only setup plan." }
} finally {
	$env:Path = $originalProcessPath
	$env:CODEX_HOME = $originalCodexHome
    [Environment]::SetEnvironmentVariable("Path", $originalUserPath, "User")
}
$failureSmokeRoot = Join-Path ([IO.Path]::GetTempPath()) ("hif-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
& (Join-Path $HiveRoot "test/integrated-installer-windows-failure-smoke.ps1") -Installer (Join-Path $HiveRoot "install-integrated.ps1") -ReleaseDir $WorkRoot -Version $releaseVersion -WorkRoot $failureSmokeRoot
Write-Host "Windows integrated installer smoke passed: $WorkRoot"
