# Integrated Windows release signing

Public Windows releases use Azure Artifact Signing (formerly Trusted Signing) with a Public Trust certificate profile. The GitHub workflow uses OpenID Connect; it does not store an Azure client secret or export a certificate private key.

This is release-maintainer configuration. It does not add a setup step for Hive users.

## One-time identity setup

1. Create an Azure Artifact Signing account, complete identity validation, and create a Public Trust certificate profile. Test profiles and self-signed certificates are not production identities.
2. Create an Entra application/service principal for GitHub Actions and add a federated credential for the GitHub environment subject:

   ```text
   repo:DavidDiaz0317/hive:environment:artifact-signing
   ```

3. Grant that principal `Artifact Signing Certificate Profile Signer` on the Artifact Signing account or the narrower certificate-profile scope.
4. Create the `artifact-signing` environment in `DavidDiaz0317/hive`. Configure **Selected branches and tags** with one tag rule, `v*-integrated.*`, and no branch rule. This prevents a manual branch workflow or modified branch from obtaining the production signing identity. Store these non-secret environment variables there:

   | Variable | Value |
   | --- | --- |
   | `AZURE_ARTIFACT_SIGNING_CLIENT_ID` | Entra application/client ID |
   | `AZURE_ARTIFACT_SIGNING_TENANT_ID` | Entra tenant ID |
   | `AZURE_ARTIFACT_SIGNING_SUBSCRIPTION_ID` | Azure subscription ID |
   | `AZURE_ARTIFACT_SIGNING_ENDPOINT` | Regional Artifact Signing endpoint |
   | `AZURE_ARTIFACT_SIGNING_ACCOUNT` | Artifact Signing account name |
   | `AZURE_ARTIFACT_SIGNING_CERTIFICATE_PROFILE` | Public Trust certificate profile name |

Do not add a client secret, PFX file, or certificate password. The workflow has `id-token: write` only in the Windows signing job and exchanges GitHub's short-lived environment-bound identity for Azure access. The signing job itself also runs only for a tag push. Manual workflow-dispatch rehearsals build an unsigned Windows fixture for smoke testing, cannot enter the signing environment, and cannot publish a release.

## Release invariant

The Windows job builds the exact `hive.exe`, checks its embedded release version and Hive commit, signs it through the pinned official Azure action, and requires a valid Authenticode code-signing certificate plus RFC3161 timestamp. Only that verified file is transferred to the Linux build job. `hive-dist` inventories the signed bytes before it writes `distribution-manifest.json`; the ZIP checksum and GitHub build provenance therefore bind the Authenticode signature.

Missing identity variables, failed OIDC authentication, signing failure, invalid certificate status, absent timestamp, changed release identity, or missing signed artifact stops the workflow before archive assembly and publication. The Ubuntu job never rebuilds the Windows executable.

After a release, independently verify the extracted executable on a normal Windows host:

```powershell
$signature = Get-AuthenticodeSignature -LiteralPath .\hive.exe
$signature | Format-List Status,StatusMessage,SignerCertificate,TimeStamperCertificate
if ($signature.Status -ne 'Valid') { throw "Hive Authenticode signature is not valid." }
.\hive.exe --version
```

## Defender false positives

Signing establishes a durable publisher identity but does not replace false-positive review for a specific behavioral detection. Submit the exact SHA-256 through Microsoft's Security Intelligence portal as a software developer, mark it clean/incorrectly detected, include the immutable release URL and successful release-workflow URL, and retain the submission ID. Do not disable Defender or add an installation exclusion as acceptance evidence.

Microsoft references:

- [Set up Artifact Signing integrations](https://learn.microsoft.com/en-us/azure/artifact-signing/how-to-signing-integrations)
- [Artifact Signing trust models](https://learn.microsoft.com/en-us/azure/artifact-signing/concept-trust-models)
- [Address Defender false positives and negatives](https://learn.microsoft.com/en-us/defender-endpoint/defender-endpoint-false-positives-negatives)
- [Submit a file for malware analysis](https://www.microsoft.com/en-us/wdsi/filesubmission)
