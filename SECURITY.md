# Security Policy

Mesh holds a team's institutional knowledge (decisions, post-mortems, and, in the
hosted/hub editions, access tokens), so we take reports seriously.

## Reporting a vulnerability

Please report security issues **privately**, not as a public GitHub issue or pull
request.

**Email security@brightinteraction.com.** That address is monitored and is the channel
we answer on. It is also published, with our PGP key, at
`https://brightinteraction.com/.well-known/security.txt`, so you can encrypt the report
if you would rather not send details in the clear.

GitHub's "Report a vulnerability" button is not available on this repository, so please
do not go looking for it. If you see it appear on a future release, it works too.

Include enough to reproduce: affected version or commit, the steps, and the impact you
observed. If you have a proof of concept, attach it.

We will acknowledge your report, keep you updated as we investigate, and credit you in
the release notes once a fix ships (unless you prefer to stay anonymous). Please give us
a reasonable window to release a fix before any public disclosure.

## Scope

This repository is the open core: the single-user vault, graph, retrieval, viewers,
CLI, MCP surface, and the sync client. The hosted hub and curator are a separate
product; vulnerabilities there can be reported the same way.

## Supported versions

Mesh is pre-1.0. Security fixes land on the latest release; please upgrade to the most
recent tag before reporting.
