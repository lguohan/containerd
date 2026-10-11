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

package sharednetns

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

// New opens the persisted namespace manager without changing any mounts.
func New(rootDir, stateDir string) (*Manager, error) {
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	return newManager(rootDir, stateDir, strings.TrimSpace(string(bootID)), linuxBackend{})
}

type linuxBackend struct{}

func (backend linuxBackend) Create(path string) (identity, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0600)
	if err != nil {
		return identity{}, err
	}
	file.Close()
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			result <- err
			return
		}
		source := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
		result <- unix.Mount(source, path, "", unix.MS_BIND, "")
	}()
	if err := <-result; err != nil {
		os.Remove(path)
		return identity{}, err
	}
	return backend.Inspect(path)
}

func (backend linuxBackend) Bind(source, target string, expected identity) error {
	sourceFile, actual, err := openNamespace(source)
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	if actual != expected {
		return fmt.Errorf("namespace source %q was replaced: %w", source, errdefs.ErrFailedPrecondition)
	}
	targetFile, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0600)
	if err != nil {
		return err
	}
	targetFile.Close()
	if err := unix.Mount(fmt.Sprintf("/proc/self/fd/%d", sourceFile.Fd()), target, "", unix.MS_BIND, ""); err != nil {
		os.Remove(target)
		return err
	}
	return nil
}

func (linuxBackend) Inspect(path string) (identity, error) {
	file, found, err := openNamespace(path)
	if err != nil {
		return identity{}, err
	}
	file.Close()
	return found, nil
}

func (linuxBackend) Remove(path string, expected identity) error {
	file, actual, err := openNamespace(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if actual != expected {
		return fmt.Errorf("refusing to unmount replaced namespace %q: %w", path, errdefs.ErrFailedPrecondition)
	}
	if err := unix.Unmount(path, unix.MNT_DETACH|unix.UMOUNT_NOFOLLOW); err != nil {
		return err
	}
	return os.Remove(path)
}

func openNamespace(path string) (*os.File, identity, error) {
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, identity{}, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(descriptor, &filesystem); err != nil {
		file.Close()
		return nil, identity{}, err
	}
	if filesystem.Type != unix.NSFS_MAGIC {
		file.Close()
		return nil, identity{}, fmt.Errorf("path %q is not an nsfs mount: %w", path, errdefs.ErrFailedPrecondition)
	}
	namespaceType, err := unix.IoctlRetInt(descriptor, unix.NS_GET_NSTYPE)
	if err != nil {
		file.Close()
		return nil, identity{}, err
	}
	if namespaceType != unix.CLONE_NEWNET {
		file.Close()
		return nil, identity{}, fmt.Errorf("path %q is not a network namespace: %w", path, errdefs.ErrFailedPrecondition)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		file.Close()
		return nil, identity{}, err
	}
	return file, identity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}
