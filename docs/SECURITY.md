<!-- SPDX-License-Identifier: GPL-3.0-or-later -->
# Security

core is a library, not a running service. It has a small security surface, and
this document states it plainly rather than dressing it up as a threat model it
does not have. The one thing worth understanding before you depend on core is
what a consumer grants when it hands core a runtime socket.

## The surface

core runs inside the consumer's process. It talks to a Docker Engine
API-compatible socket to do its job: list and inspect containers, watch
lifecycle events, read compose labels, and stop, start, or exec containers. The
optional `Provisioner` capability also creates throwaway objects (images,
networks, volumes, containers) so a consumer can prove a restore in a sandbox.

That socket is the whole story. Access to a container-runtime socket is
effectively root on the host: whoever can drive it can start a privileged
container, bind-mount the host filesystem, and take over the machine. core does
not widen that grant and cannot narrow it. When a consumer gives core socket
access, the consumer is granting broad host control, and the decision to grant
it lives with the consumer, not here.

core itself holds nothing sensitive. It stores no secrets and no key material,
keeps no datastore, and makes no network calls of its own. Its only outbound
traffic is the runtime API over the socket the consumer points it at, and its
only non-stdlib dependency is the moby Engine API client.

A few boundaries are the consumer's to hold, not core's:

- core does not decide who may reach the socket. It uses whatever socket the
  consumer opens, with whatever privilege that socket carries.
- `Provisioner` creates networks internal-only (no external routing) and never
  publishes ports, but the caller owns teardown. Every create spec carries a
  `Labels` map so orphans left by a crash can be found and swept. Sweeping them
  is the consumer's job.
- core does not sandbox the consumer or verify that the socket is the runtime it
  claims to be. It trusts the endpoint it is given.

None of this is a residual to apologize for. A runtime-abstraction library that
talks to a socket has exactly this surface, and the honest posture is to name
the socket grant as the boundary and stop there.

## Engine API client and its dependency posture

core drives the moby split-out Engine API client (`github.com/moby/moby/client`
and `github.com/moby/moby/api`), the supported normal-semver replacements for the
frozen `github.com/docker/docker` monolith. As of v0.9.0 the `docker/docker`
dependency is gone.

That migration removed the two advisories that used to be flagged here:

- GO-2026-4887 / CVE-2026-34040: a Moby AuthZ plugin bypass on oversized request
  bodies.
- GO-2026-4883 / CVE-2026-33997: an off-by-one error in Moby plugin-privilege
  validation.

Both flaws lived in the Docker daemon, not in a client, so neither was ever
reachable in core (which runs no daemon, installs no plugins, and configures no
authorization plugins). They read "Fixed in: N/A" only because the fix went to
the split-out modules rather than the frozen one, so a scanner flagged any import
of `docker/docker` regardless. Moving to the maintained client modules drops the
dependency and the findings outright: `govulncheck ./...` in the go1.25 toolchain
now reports neither.

## Supported API-version floor

The moby client enforces a minimum Engine API version of `1.40`
(`MinAPIVersion`). Below that floor the client returns an error on the first call
rather than clamping to a lower version, so:

- Podman 1.x and 2.x, and Docker older than 19.03, are no longer supported and
  fail fast on first use.
- Current Docker and supported Podman (compat API 1.40 at Podman v3.4/v4.0 and
  higher after) are unaffected.

This is a deliberate consequence of the client, not a policy core imposes on top,
and it is stated here so a consumer pointing core at a very old daemon knows why
the first call errors.

## Reporting a vulnerability

Report a suspected vulnerability through GitHub's private vulnerability
reporting on this repository: open the Security tab and choose "Report a
vulnerability". That keeps the report private while it is triaged.

We work to coordinated disclosure: a fix is prepared and released before the
details are made public, and we will keep you in the loop on timing.
