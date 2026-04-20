//go:build linux

// Teleport
// Copyright (C) 2026 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package reexecsftp

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openFileNoFollow(file string, flags int, mode os.FileMode) (*os.File, error) {
	absFile, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	how := &unix.OpenHow{
		Flags:   uint64(flags),
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	}
	if flags&os.O_CREATE != 0 {
		how.Mode = uint64(mode)
	}
	fd, err := unix.Openat2(0 /* dirfd, ignored */, absFile, how)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), file), nil
}
