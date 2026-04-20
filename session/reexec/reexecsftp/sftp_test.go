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
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/session/sftputils"
)

func TestEnsureReqIsAllowed(t *testing.T) {
	t.Parallel()
	const filePath = "/foo/bar/baz.txt"
	passTests := []struct {
		name    string
		allowed *allowedOps
		req     *sftp.Request
	}{
		{
			name: "no restrictions",
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodGet,
			},
		},
		{
			name:    "read",
			allowed: &allowedOps{path: filePath},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodGet,
			},
		},
		{
			name:    "write",
			allowed: &allowedOps{path: filePath, write: true},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodPut,
			},
		},
		{
			name:    "chmod",
			allowed: &allowedOps{path: filePath, write: true},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodSetStat,
			},
		},
		{
			name:    "stat in read mode",
			allowed: &allowedOps{path: filePath},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodStat,
			},
		},
		{
			name:    "lstat in read mode",
			allowed: &allowedOps{path: filePath},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodLstat,
			},
		},
		{
			name:    "stat in write mode",
			allowed: &allowedOps{path: filePath, write: true},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodStat,
			},
		},
		{
			name:    "lstat in write mode",
			allowed: &allowedOps{path: filePath, write: true},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodLstat,
			},
		},
	}
	for _, tc := range passTests {
		t.Run("allow "+tc.name, func(t *testing.T) {
			tc.req.Filepath = filePath
			if tc.allowed != nil {
				tc.allowed.path = filePath
			}
			handler := &sftpHandler{allowed: tc.allowed}
			require.NoError(t, handler.ensureReqIsAllowed(tc.req))
		})
	}

	const convolutedPath = "/foo/bar/../bar/baz.txt"
	failTests := []struct {
		name    string
		allowed *allowedOps
		req     *sftp.Request
	}{
		{
			name:    "uncleaned path",
			allowed: &allowedOps{path: convolutedPath},
			req: &sftp.Request{
				Filepath: convolutedPath,
				Method:   sftputils.MethodGet,
			},
		},
		{
			name:    "get in write mode",
			allowed: &allowedOps{path: filePath, write: true},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodGet,
			},
		},
		{
			name:    "write in read mode",
			allowed: &allowedOps{path: filePath},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodPut,
			},
		},
		{
			name:    "chmod in read mode",
			allowed: &allowedOps{path: filePath},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodSetStat,
			},
		},
		{
			name:    "unknown method",
			allowed: &allowedOps{path: filePath},
			req: &sftp.Request{
				Filepath: filePath,
				Method:   sftputils.MethodRename,
			},
		},
	}
	for _, tc := range failTests {
		t.Run("deny "+tc.name, func(t *testing.T) {
			handler := &sftpHandler{allowed: tc.allowed}
			require.Error(t, handler.ensureReqIsAllowed(tc.req))
		})
	}
}

func TestOpenFile(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	tempRoot, err := filepath.EvalSymlinks(tempDir)
	require.NoError(t, err)

	fileData := []byte("data")
	file := filepath.Join(tempRoot, "foo.txt")
	require.NoError(t, os.WriteFile(file, fileData, 0o644))
	link := filepath.Join(tempRoot, "link")
	require.NoError(t, os.Symlink(tempRoot, link))
	linkTarget := filepath.Join(link, "foo.txt")

	tests := []struct {
		name    string
		path    string
		allowed *allowedOps
		assert  assert.ErrorAssertionFunc
	}{
		{
			name:   "regular read",
			path:   file,
			assert: assert.NoError,
		},
		{
			name:    "moderated read",
			path:    file,
			allowed: &allowedOps{path: file},
			assert:  assert.NoError,
		},
		{
			name:   "symlink read",
			path:   linkTarget,
			assert: assert.NoError,
		},
		{
			name:    "moderated symlink read",
			path:    linkTarget,
			allowed: &allowedOps{path: linkTarget},
			assert:  assert.Error,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := sftp.NewRequest(sftputils.MethodGet, tc.path)
			handler := &sftpHandler{
				allowed: tc.allowed,
			}
			file, err := handler.openFile(req)
			tc.assert(t, err)
			if file == nil {
				return
			}
			gotData := make([]byte, len(fileData))
			_, err = file.ReadAt(gotData, 0)
			assert.NoError(t, err)
			assert.Equal(t, fileData, gotData)
			if closer, ok := file.(io.Closer); ok {
				assert.NoError(t, closer.Close())
			}
		})
	}
}
