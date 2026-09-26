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
only non-stdlib dependency is the Docker SDK.

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

## Known advisories against the Docker SDK dependency

core currently depends on `github.com/docker/docker` (the moby module) for its
Engine API client. Two advisories are reported against that module:

- GO-2026-4887 / CVE-2026-34040: a Moby AuthZ plugin bypass on oversized request
  bodies.
- GO-2026-4883 / CVE-2026-33997: an off-by-one error in Moby plugin-privilege
  validation.

Neither is reachable in core, and the reason is structural. Both flaws live in
the Docker daemon: one in the daemon's authorization-plugin path, the other in
the daemon's plugin-install privilege validation. core is a pure API client. It
runs no daemon, installs no plugins, and configures no authorization plugins, so
it never executes either vulnerable code path. A source-level reachability scan
(`govulncheck ./...`) confirms that core reaches these advisories only at the
module-presence level, through ordinary client calls and package init, not
through the vulnerable symbols. The advisory records carry no symbol data, which
is why a dependency scanner cannot narrow them automatically and flags any import
of the module regardless of which functions the caller uses.

These advisories also read "Fixed in: N/A" because the `github.com/docker/docker`
module is frozen. The fix landed in Docker's split-out client modules
(`github.com/moby/moby/client` and `github.com/moby/moby/api`). A planned release
migrates core onto those modules, which removes the dependency and the finding
outright. This section documents the waiver for the window until that migration
lands: the advisories are present in the dependency graph but unreachable in a
pure client, so they are accepted here with this justification rather than
suppressed silently.

## Reporting a vulnerability

Report a suspected vulnerability through GitHub's private vulnerability
reporting on this repository: open the Security tab and choose "Report a
vulnerability". That keeps the report private while it is triaged.

We work to coordinated disclosure: a fix is prepared and released before the
details are made public, and we will keep you in the loop on timing.
