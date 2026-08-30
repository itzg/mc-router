# Security Policy

## Supported Versions

Only the latest revision of the main branch receives security updates. If you discover a vulnerability, please test against the latest release/image version before reporting.

## Dependency Vulnerabilities & Automated Scans

* **No Security Reports or PRs for Dependencies:** Please **do not** submit security reports, advisories, issues, or pull requests for vulnerable third-party dependencies or upstream base packages.
* **Sole Maintainer & Automated Updates:** This repository is maintained by a single person. To manage maintenance overhead and focus on core features, this project relies entirely on **Dependabot** (and automated workflows) to submit updates for dependencies, base images, and toolchains.
* **Overdue Dependabot PRs:** If Dependabot has already opened a Pull Request for a dependency update that appears to be stuck or forgotten, feel free to leave a polite reminder comment directly on that Dependabot PR. Please do not open duplicate PRs or separate issues.

## Reporting a Vulnerability

> [!IMPORTANT]
> Please do not report security vulnerabilities through public GitHub issues, discussions, or pull requests.

To report a vulnerability in this project's custom codebase privately:

1. Navigate to the **Security** tab of this repository.
2. Select **Report a vulnerability** to open a private security advisory.

This allows me to review, reproduce, and resolve the issue in a private environment before public disclosure.

## Important Expectations & Bug Bounties

* **No Financial Bounties:** This is an open-source, community-maintained project. Financial rewards or monetary bounties are not offered for vulnerability reports.
* **Automated & Unactionable Reports:** Reports generated solely by automated scanners without a working, practical proof-of-concept demonstrating an exploitable vulnerability in this repository's custom code will be closed as unactionable.
