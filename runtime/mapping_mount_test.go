// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// mapMountPoint decides how a container's storage is represented to a
// consumer, and a consumer's backup or archive step keys off that Mount
// shape. A silent miscarry (a volume mapped as a bind, a read-only mount
// reported writable, a lost source path) sends a data-critical action at the
// wrong target rather than raising an error, so the mapping is locked here by
// invariant over the input space rather than by one happy-path example.
//
// Closes Vikunja #511.

// TestMapMountPointType proves the Type mapping across every mount kind the
// engine reports, including the load-bearing default: mapMountPoint treats
// anything that is not a recognized bind or tmpfs as a named volume, because
// a volume is the case a consumer most needs to get right (it is what gets
// dumped or archived). A future engine mount kind must therefore fall to
// MountVolume, never to the empty string.
func TestMapMountPointType(t *testing.T) {
	cases := []struct {
		name string
		in   mount.Type
		want MountType
	}{
		{"bind", mount.TypeBind, MountBind},
		{"tmpfs", mount.TypeTmpfs, MountTmpfs},
		{"volume", mount.TypeVolume, MountVolume},
		// The empty type an unpopulated summary can carry must not become an
		// empty MountType. It defaults to volume.
		{"empty defaults to volume", mount.Type(""), MountVolume},
		// An unrecognized (future or non-Linux) mount kind must also default
		// to volume rather than leaking through as an unknown MountType.
		{"unknown defaults to volume", mount.Type("some-future-kind"), MountVolume},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mapMountPoint(container.MountPoint{Type: c.in})
			if got.Type != c.want {
				t.Fatalf("mapMountPoint(Type=%q).Type = %q, want %q", c.in, got.Type, c.want)
			}
		})
	}
}

// TestMapMountPointReadOnlyIsInvertedRW pins the one field the mapper
// transforms rather than copies: core's Mount.ReadOnly is the negation of the
// engine's MountPoint.RW. Getting this backwards would report a read-only
// mount as writable (or the reverse), so both directions are asserted.
func TestMapMountPointReadOnlyIsInvertedRW(t *testing.T) {
	cases := []struct {
		rw           bool
		wantReadOnly bool
	}{
		{rw: true, wantReadOnly: false},
		{rw: false, wantReadOnly: true},
	}
	for _, c := range cases {
		got := mapMountPoint(container.MountPoint{Type: mount.TypeBind, RW: c.rw})
		if got.ReadOnly != c.wantReadOnly {
			t.Fatalf("mapMountPoint(RW=%v).ReadOnly = %v, want %v", c.rw, got.ReadOnly, c.wantReadOnly)
		}
	}
}

// TestMapMountPointCarriesFields walks representative real mounts through the
// mapper and asserts the whole resulting Mount shape, so the fields the
// mapper copies verbatim (Name, Source, Destination) are proven carried for a
// named volume, an anonymous volume, a data-looking bind, and a tmpfs. The
// tmpfs and bind cases also lock the engine's own conventions the mapper
// preserves: a tmpfs mount has no source and no name, and a bind has a source
// but no name.
func TestMapMountPointCarriesFields(t *testing.T) {
	cases := []struct {
		name string
		in   container.MountPoint
		want Mount
	}{
		{
			name: "named volume",
			in: container.MountPoint{
				Type:        mount.TypeVolume,
				Name:        "pgdata",
				Source:      "/var/lib/docker/volumes/pgdata/_data",
				Destination: "/var/lib/postgresql/data",
				RW:          true,
			},
			want: Mount{
				Type:        MountVolume,
				Name:        "pgdata",
				Source:      "/var/lib/docker/volumes/pgdata/_data",
				Destination: "/var/lib/postgresql/data",
				ReadOnly:    false,
			},
		},
		{
			// An anonymous volume still carries a name (the engine's hex id).
			// The mapper must carry it verbatim, since that id is how the
			// volume is addressed.
			name: "anonymous volume",
			in: container.MountPoint{
				Type:        mount.TypeVolume,
				Name:        "3b2a1c9f8e7d6c5b4a3928170615243342516078",
				Source:      "/var/lib/docker/volumes/3b2a1c9f8e7d6c5b4a3928170615243342516078/_data",
				Destination: "/data",
				RW:          true,
			},
			want: Mount{
				Type:        MountVolume,
				Name:        "3b2a1c9f8e7d6c5b4a3928170615243342516078",
				Source:      "/var/lib/docker/volumes/3b2a1c9f8e7d6c5b4a3928170615243342516078/_data",
				Destination: "/data",
				ReadOnly:    false,
			},
		},
		{
			// A bind whose destination looks like a data directory, mounted
			// read-only. A bind has a host-side source and no name.
			name: "read-only data bind",
			in: container.MountPoint{
				Type:        mount.TypeBind,
				Name:        "",
				Source:      "/srv/app/data",
				Destination: "/var/lib/app/data",
				RW:          false,
			},
			want: Mount{
				Type:        MountBind,
				Name:        "",
				Source:      "/srv/app/data",
				Destination: "/var/lib/app/data",
				ReadOnly:    true,
			},
		},
		{
			// A tmpfs mount has no source and no name.
			name: "tmpfs",
			in: container.MountPoint{
				Type:        mount.TypeTmpfs,
				Name:        "",
				Source:      "",
				Destination: "/run",
				RW:          true,
			},
			want: Mount{
				Type:        MountTmpfs,
				Name:        "",
				Source:      "",
				Destination: "/run",
				ReadOnly:    false,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mapMountPoint(c.in)
			if got != c.want {
				t.Fatalf("mapMountPoint(%s):\n got  %+v\n want %+v", c.name, got, c.want)
			}
		})
	}
}
