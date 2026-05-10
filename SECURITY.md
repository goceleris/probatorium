# Security

probatorium is a benchmarking and validation harness; it is not a runtime library and does not ship in any production application path. It exists only to drive load and check invariants against a separate engine (celeris).

For security issues affecting **celeris**, the engine under test, see https://github.com/goceleris/celeris/security/policy.

If you find an issue specific to probatorium itself (e.g. the orchestrator or fuzzer leaks credentials, the ansible playbooks leave a host in an unsafe state, etc.), please open a private security advisory at https://github.com/goceleris/probatorium/security/advisories/new rather than a public issue.
