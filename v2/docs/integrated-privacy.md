# Integrated privacy and data handling

Hive + Visual Hive is open-source repository testing and repair automation. The
project does not operate a hosted Hive service and does not add product
telemetry, advertising identifiers, or project-controlled analytics to the
integrated command-line distribution.

## Data processed by the installed product

The installed tools may process repository source, test output, screenshots,
issue and pull-request content, Git metadata, workflow results, and local Hive
state in order to perform the actions the user requests. State is stored on the
user's machine or on infrastructure and repositories controlled by the user.
Visual Hive keeps deterministic evidence local or in the user's configured
GitHub Actions artifacts and repositories.

Hive connects to GitHub through the user's authenticated GitHub CLI session.
When the user explicitly configures an AI repair provider, Hive can send the
bounded prompt and repository context required for that repair to the selected
provider. GitHub and the selected provider process that data under their own
terms and privacy policies; users should not enable a provider for material
they are not authorized to send to it.

The release workflow uses GitHub Actions, GitHub artifact attestations, and,
after approval, SignPath.io for Windows code signing. Those services receive
the source-control, build, account, and artifact information needed to provide
their services under their respective privacy policies.

## Credentials and secrets

The integrated installer and release do not collect users' GitHub or provider
credentials for the project maintainers. Credentials remain in the stores
managed by GitHub CLI, the selected provider, the operating system, or the
user's CI platform. Hive is designed to avoid placing credentials in generated
issues, pull requests, evidence bundles, or release artifacts.

Users should immediately revoke a credential that is accidentally committed or
published and follow the affected provider's incident-response process.

## Retention and deletion

Local Hive state remains until the user removes the installation or state
directory. GitHub issues, pull requests, workflow logs, and artifacts follow
the retention settings of the user's GitHub repository. AI-provider data
follows the provider account and retention settings selected by the user. The
Hive project cannot delete data held in a user's local environment, GitHub
account, or third-party provider account.

Questions or privacy defects can be reported in the repository issue tracker.
Sensitive reports should use GitHub's private security-advisory interface.

