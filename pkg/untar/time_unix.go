//go:build !windows

// From https://github.com/containerd/containerd/blob/v1.7.3/archive/time_unix.go .
/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package untar

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// chtimes is from https://github.com/containerd/containerd/blob/v1.7.3/archive/time_unix.go#L28-L38 .
func chtimes(path string, atime, mtime time.Time) error {
	var utimes [2]unix.Timespec
	utimes[0] = unix.NsecToTimespec(atime.UnixNano())
	utimes[1] = unix.NsecToTimespec(mtime.UnixNano())

	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, utimes[0:], unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("failed call to UtimesNanoAt for %s: %w", path, err)
	}

	return nil
}
