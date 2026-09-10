# Security policy

## Reporting a vulnerability

Do not open a public GitHub issue for security problems.

Report vulnerabilities privately through
[GitHub private vulnerability reporting](https://github.com/amitbet/dynapp-agent/security/advisories/new).
Include the agent version or commit, a reproduction, and the impact you believe
it has.

You should receive an acknowledgement within 5 working days.

## Scope

In scope: the DynApp agent loopback handshake, browser pairing, capability
enforcement, HTTP connect allowlists, and update verification.

Out of scope: third-party apps published by other accounts, denial of service
against your own machine through capabilities you explicitly approved, and
issues that require a compromised operating-system account.

## Security model

Apps declare every capability in `app.json`. The agent shows a pairing prompt
per browser identity and origin, grants only the approved groups, and enforces
them on every request. Local use never requires a Dyner account.

## Supported versions

Only the latest GitHub release receives fixes. Release builds update themselves
from signed assets; keep them current.
