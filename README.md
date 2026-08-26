# tagwright/core

Shared building blocks for the tagwright suite. The first piece is `runtime`, a
container-runtime abstraction over Docker and Podman. It talks to a Docker Engine
API-compatible socket and exposes the operations the suite's tools need: list and
inspect containers, watch the socket for lifecycle events, read compose
project/service labels, stop and start containers, exec into them, and a set of
normalized Container and Mount types so callers do not deal in engine-specific
shapes. A single request-and-mapping core is shared by a Docker adapter and a
Podman adapter that differ only in their default socket path and how they read
compose identity off a container's labels.

This module is a leaf: its only non-stdlib dependency is the Docker SDK. It was
extracted from ballast so a second consumer can share the same abstraction.

Licensed under GPL-3.0-or-later. See LICENSE.
