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
$releaseVersion = "vlocal-windows"

New-Item -ItemType Directory -Path $WorkRoot -Force | Out-Null
$distribution = Join-Path $WorkRoot "hive-integrated-$releaseVersion-windows-amd64"
Push-Location $HiveRoot
try {
    go build -trimpath -ldflags "-s -w -X main.gitHash=$hiveCommit -X main.gitShort=$($hiveCommit.Substring(0,12))" -o (Join-Path $WorkRoot "hive.exe") ./cmd/hive
    go run ./cmd/hive-dist --hive (Join-Path $WorkRoot "hive.exe") --hive-commit $hiveCommit --visual-hive $VisualBundle --visual-hive-commit $visualCommit --node $node --node-version $nodeVersion --skill (Join-Path $HiveRoot "skills/hive") --target-os windows --target-arch amd64 --output $distribution | Out-Null
} finally {
    Pop-Location
}
$asset = Join-Path $WorkRoot "hive-integrated-$releaseVersion-windows-amd64.zip"
Compress-Archive -LiteralPath $distribution -DestinationPath $asset -CompressionLevel Optimal
$hash = (Get-FileHash -LiteralPath $asset -Algorithm SHA256).Hash.ToLowerInvariant()
Set-Content -LiteralPath "$asset.sha256" -Value "$hash  $([IO.Path]::GetFileName($asset))" -Encoding ascii

& (Join-Path $HiveRoot "install-integrated.ps1") -Version $releaseVersion -ReleaseDir $WorkRoot -SkipAttestation -NoPath -NoCodexSkill -InstallDir (Join-Path $WorkRoot "installed")
& (Join-Path $WorkRoot "installed/runtime/node.exe") (Join-Path $WorkRoot "installed/visual-hive/visual-hive.mjs") --version | Out-Null
$plan = & (Join-Path $WorkRoot "installed/hive.exe") setup --repo DavidDiaz0317/visual-hive-demo-site --coverage comprehensive --automation advisory --provider codex --visual-hive --plan --state-dir (Join-Path $WorkRoot "state") --json | ConvertFrom-Json
if ($plan.plan.schema_version -ne "hive.setup-plan.v1" -or -not $plan.plan.read_only) { throw "Installed Hive did not produce a read-only setup plan." }
& (Join-Path $HiveRoot "test/integrated-installer-windows-failure-smoke.ps1") -Installer (Join-Path $HiveRoot "install-integrated.ps1") -ReleaseDir $WorkRoot -Version $releaseVersion -WorkRoot (Join-Path $WorkRoot "failure-smoke")
Write-Host "Windows integrated installer smoke passed: $WorkRoot"
