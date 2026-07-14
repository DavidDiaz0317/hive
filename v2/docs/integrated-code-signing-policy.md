# Integrated code-signing policy

This policy applies to public Windows executables in the Hive + Visual Hive
integrated distribution published from `DavidDiaz0317/hive`. It does not cover
unofficial builds, source archives, or artifacts produced by another fork.

## Signing identity and key custody

The project is applying to the SignPath Foundation open-source program. Once
approved, official Windows executables will be Authenticode-signed through
SignPath.io with the SignPath Foundation certificate. SignPath retains the
private signing key; project maintainers do not download, export, or store it.

The project will not describe a Windows executable as signed until its
Authenticode signature and RFC 3161 timestamp have both been verified. The
current `v0.4.1-integrated.16` Windows executable predates this policy and is
unsigned.

## Covered artifacts

The initial signing scope is the `hive.exe` PE executable contained in the
Windows x64 integrated release archive. Scripts, JavaScript bundles, Linux
binaries, and third-party runtime files are authenticated by the release
manifest, SHA-256 checksums, and GitHub build-provenance attestations rather
than an Authenticode signature from this project.

## Build and approval policy

1. A release starts from an immutable `v*-integrated.*` tag in
   `DavidDiaz0317/hive`.
2. The public GitHub Actions workflow builds on GitHub-hosted runners. A signing
   request must originate from that release workflow and exact tag commit.
3. SignPath source-control and build-system integration must make the submitted
   executable verifiably attributable to the public source and workflow.
4. An explicitly assigned project approver reviews every signing request. The
   request must identify the release tag, commit, workflow run, and unsigned
   SHA-256. Automatic approval is not permitted.
5. The workflow verifies the returned Authenticode signature, certificate
   chain, timestamp, embedded Hive version, and embedded Hive commit before it
   assembles the release archive.
6. The distribution manifest inventories the signed bytes. The release then
   publishes checksums and GitHub build-provenance attestations and verifies
   GitHub release immutability.

A missing approval, untrusted source, signature error, version mismatch,
manifest mismatch, checksum error, failed attestation, or mutable release
stops publication. Maintainers must not manually sign or substitute a binary
outside the reviewed release workflow.

## Release identity and verification

Users can run:

```text
hive --version
```

to read the integrated release version, exact Hive commit, and bundled Visual
Hive version without relying on a source checkout. Windows users should also
verify the extracted executable:

```powershell
$signature = Get-AuthenticodeSignature -LiteralPath .\hive.exe
$signature | Format-List Status,StatusMessage,SignerCertificate,TimeStamperCertificate
if ($signature.Status -ne "Valid") { throw "Hive Authenticode signature is not valid." }
```

The installer independently verifies the archive SHA-256, distribution
manifest, exact tag commit, GitHub-hosted workflow identity, and build
provenance before activation.

## Maintainers and incident response

The repository owner is responsible for assigning SignPath submitter and
approver roles with least privilege, enforcing multi-factor authentication,
and removing access that is no longer required. Role assignments and signing
requests are auditable in SignPath.

Suspected key, account, workflow, or artifact compromise must stop publication.
Maintainers will preserve the affected evidence, revoke or suspend the relevant
SignPath access, contact SignPath Foundation when certificate action is
required, publish a GitHub security advisory or release notice as appropriate,
and issue a new immutable release only after the cause has been corrected and
all release gates pass.

Report suspected signing or release-integrity problems through the repository's
GitHub security advisory interface. Do not include credentials or private keys
in a public issue.

