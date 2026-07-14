# Integrated downloads and verification

Official Hive + Visual Hive integrated releases are published from the
maintained [`DavidDiaz0317/hive` release page](https://github.com/DavidDiaz0317/hive/releases).
Each release provides Windows x64 and Linux x64 archives, SHA-256 checksums, an
inventory manifest, and GitHub build-provenance attestations. Follow the
[integrated quickstart](integrated-quickstart.md) so the installer verifies the
tag, workflow identity, attestation, checksum, and manifest before activation.

## Windows signing status

The project is applying to the free SignPath Foundation open-source program for
Authenticode signing. After approval, the official GitHub Actions release
workflow will use SignPath Foundation for code signing and will fail closed
unless the returned `hive.exe` signature and timestamp verify before archive
assembly. See the [integrated code-signing policy](integrated-code-signing-policy.md).

The current `v0.4.1-integrated.16` Windows executable is **unsigned**. It was
built by the public workflow, is covered by the release manifest, checksum, and
GitHub build-provenance attestation, and is undergoing Microsoft Defender
false-positive review. Do not interpret those controls as an Authenticode
publisher signature. A future release will be identified as signed only after
the policy's verification gates pass.

The integrated distribution's [privacy and data-handling notice](integrated-privacy.md)
explains what the installed tools process and which user-selected external
services they can contact.

