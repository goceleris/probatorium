# Security

probatorium is a benchmarking and validation harness; it is not a runtime library and does not ship in any production application path. It exists only to drive load and check invariants against a separate engine (celeris).

## Reporting a vulnerability

**Do not open a public issue for security problems.** Report privately through one of these channels, preferred first:

1. The repository's **Security** tab → **Report a vulnerability** (private vulnerability reporting is enabled): https://github.com/goceleris/probatorium/security/advisories/new
2. Email **security@goceleris.dev**

Include a description, steps to reproduce, the impact you believe it has, and a suggested fix if you have one. We acknowledge every report within **72 hours** and keep you informed while it is triaged and fixed.

## Scope

Issues specific to probatorium itself — for example the orchestrator or fuzzer leaking credentials, the ansible playbooks leaving a host in an unsafe state, or a workflow that lets untrusted code reach the self-hosted cluster — belong here.

For security issues affecting **celeris**, the engine under test, use the engine's own policy instead: https://github.com/goceleris/celeris/security/policy (celeris/SECURITY.md). It also lists the celeris versions that receive security fixes.
