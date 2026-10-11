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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errInjected = errors.New("injected namespace failure")

type fakeBackend struct {
	namespaces map[string]identity
	nextInode  uint64
	creates    int
	createErr  error
	bindErr    error
	removeErr  error
}

func (backend *fakeBackend) Create(path string) (identity, error) {
	if backend.createErr != nil {
		return identity{}, backend.createErr
	}
	if _, exists := backend.namespaces[path]; exists {
		return identity{}, os.ErrExist
	}
	backend.nextInode++
	backend.creates++
	found := identity{Device: 4, Inode: backend.nextInode}
	backend.namespaces[path] = found
	return found, nil
}

func (backend *fakeBackend) Bind(source, target string, expected identity) error {
	if backend.bindErr != nil {
		return backend.bindErr
	}
	actual, err := backend.Inspect(source)
	if err != nil {
		return err
	}
	if actual != expected {
		return errdefs.ErrFailedPrecondition
	}
	if _, exists := backend.namespaces[target]; exists {
		return os.ErrExist
	}
	backend.namespaces[target] = expected
	return nil
}

func (backend *fakeBackend) Inspect(path string) (identity, error) {
	found, exists := backend.namespaces[path]
	if !exists {
		return identity{}, os.ErrNotExist
	}
	return found, nil
}

func (backend *fakeBackend) Remove(path string, expected identity) error {
	if backend.removeErr != nil {
		return backend.removeErr
	}
	found, exists := backend.namespaces[path]
	if !exists {
		return nil
	}
	if found != expected {
		return errdefs.ErrFailedPrecondition
	}
	delete(backend.namespaces, path)
	return nil
}

func setupManager(t *testing.T) (*Manager, *fakeBackend) {
	t.Helper()
	backend := &fakeBackend{namespaces: map[string]identity{}}
	manager, err := newManager(t.TempDir(), t.TempDir(), "test-boot", backend)
	require.NoError(t, err)
	return manager, backend
}

func restartManager(t *testing.T, manager *Manager, backend *fakeBackend) *Manager {
	t.Helper()
	restarted, err := newManager(manager.rootDir, manager.stateDir, manager.bootID, backend)
	require.NoError(t, err)
	return restarted
}

func TestAcquireIsolationAndRetention(t *testing.T) {
	manager, backend := setupManager(t)
	first, err := manager.Acquire("worker.class", "sandbox-one")
	require.NoError(t, err)
	second, err := manager.Acquire("worker.class", "sandbox-two")
	require.NoError(t, err)
	other, err := manager.Acquire("other-class", "sandbox-other")
	require.NoError(t, err)
	assert.Equal(t, first.Inode, second.Inode)
	assert.Equal(t, first.CanonicalPath, second.CanonicalPath)
	assert.NotEqual(t, first.Path, second.Path)
	assert.NotEqual(t, first.Inode, other.Inode)
	assert.Equal(t, 2, backend.creates)
	again, err := manager.Acquire("worker.class", "sandbox-one")
	require.NoError(t, err)
	assert.Equal(t, first, again)
	_, err = manager.Acquire("other-class", "sandbox-one")
	assert.ErrorIs(t, err, errdefs.ErrAlreadyExists)
	require.NoError(t, manager.Release(first))
	require.NoError(t, manager.Release(first))
	require.NoError(t, manager.Validate(second))
	require.NoError(t, manager.Release(second))
	assert.Contains(t, backend.namespaces, first.CanonicalPath)
	manager = restartManager(t, manager, backend)
	later, err := manager.Acquire("worker.class", "sandbox-later")
	require.NoError(t, err)
	assert.Equal(t, first.Inode, later.Inode)
	assert.Equal(t, first.Generation, later.Generation)
	assert.Equal(t, 2, backend.creates)
}

func TestAcquireConcurrent(t *testing.T) {
	manager, backend := setupManager(t)
	const count = 24
	var workers sync.WaitGroup
	members := make([]Membership, count)
	for index := range count {
		workers.Go(func() {
			member, err := manager.Acquire("worker", fmt.Sprintf("sandbox-%d", index))
			assert.NoError(t, err)
			members[index] = member
		})
	}
	workers.Wait()
	assert.Equal(t, 1, backend.creates)
	for _, member := range members {
		assert.Equal(t, members[0].Inode, member.Inode)
	}
	for _, member := range members {
		workers.Go(func() {
			assert.NoError(t, manager.Release(member))
		})
	}
	workers.Wait()
	assert.Len(t, backend.namespaces, 1)
	assert.Empty(t, manager.groups["worker"].Members)
}

func TestRecoverReconcilesOrphans(t *testing.T) {
	manager, backend := setupManager(t)
	live, err := manager.Acquire("worker", "sandbox-live")
	require.NoError(t, err)
	orphan, err := manager.Acquire("worker", "sandbox-orphan")
	require.NoError(t, err)
	manager = restartManager(t, manager, backend)
	require.NoError(t, manager.Recover([]Membership{live}))
	require.NoError(t, manager.Validate(live))
	assert.NotContains(t, backend.namespaces, orphan.Path)
	assert.Contains(t, backend.namespaces, orphan.CanonicalPath)
	manager = restartManager(t, manager, backend)
	require.NoError(t, manager.Recover(nil))
	assert.Len(t, backend.namespaces, 1)
	later, err := manager.Acquire("worker", "sandbox-later")
	require.NoError(t, err)
	assert.Equal(t, live.Inode, later.Inode)
}

func TestAcquireFailuresAreRetryable(t *testing.T) {
	t.Run("creation", func(t *testing.T) {
		manager, backend := setupManager(t)
		backend.createErr = errInjected
		_, err := manager.Acquire("worker", "sandbox")
		require.ErrorIs(t, err, errInjected)
		backend.createErr = nil
		manager = restartManager(t, manager, backend)
		member, err := manager.Acquire("worker", "sandbox")
		require.NoError(t, err)
		require.NoError(t, manager.Validate(member))
		assert.Equal(t, 1, backend.creates)
	})
	t.Run("creation-intent-write", func(t *testing.T) {
		manager, backend := setupManager(t)
		manager.write = func(string, []byte) error { return errInjected }
		_, err := manager.Acquire("worker", "sandbox")
		require.ErrorIs(t, err, errInjected)
		assert.Empty(t, manager.groups)
		assert.Empty(t, backend.namespaces)
		manager.write = writeManifest
		_, err = manager.Acquire("worker", "sandbox")
		require.NoError(t, err)
	})
	t.Run("membership-write", func(t *testing.T) {
		manager, backend := setupManager(t)
		writes := 0
		manager.write = func(path string, data []byte) error {
			writes++
			if writes == 3 {
				return errInjected
			}
			return writeManifest(path, data)
		}
		_, err := manager.Acquire("worker", "sandbox")
		require.ErrorIs(t, err, errInjected)
		assert.Len(t, backend.namespaces, 1)
		member, err := manager.Acquire("worker", "sandbox")
		require.NoError(t, err)
		assert.Equal(t, 1, backend.creates)
		manager = restartManager(t, manager, backend)
		require.NoError(t, manager.Validate(member))
	})
	t.Run("canonical-write", func(t *testing.T) {
		manager, backend := setupManager(t)
		writes := 0
		manager.write = func(path string, data []byte) error {
			writes++
			if writes == 2 {
				return errInjected
			}
			return writeManifest(path, data)
		}
		_, err := manager.Acquire("worker", "sandbox")
		require.ErrorIs(t, err, errInjected)
		assert.Empty(t, backend.namespaces)
		manager = restartManager(t, manager, backend)
		member, err := manager.Acquire("worker", "sandbox")
		require.NoError(t, err)
		require.NoError(t, manager.Validate(member))
		assert.Equal(t, 2, backend.creates)
	})
	t.Run("alias-bind", func(t *testing.T) {
		manager, backend := setupManager(t)
		backend.bindErr = errInjected
		_, err := manager.Acquire("worker", "sandbox")
		require.ErrorIs(t, err, errInjected)
		backend.bindErr = nil
		assert.Empty(t, manager.groups["worker"].Members)
		manager = restartManager(t, manager, backend)
		require.NoError(t, manager.Recover(nil))
		assert.Len(t, backend.namespaces, 1)
		_, err = manager.Acquire("worker", "sandbox")
		require.NoError(t, err)
		assert.Equal(t, 1, backend.creates)
	})
}

func TestIncompleteCreationFailsClosedAfterRestart(t *testing.T) {
	manager, backend := setupManager(t)
	backend.removeErr = errInjected
	writes := 0
	manager.write = func(path string, data []byte) error {
		writes++
		if writes > 1 {
			return errInjected
		}
		return writeManifest(path, data)
	}
	_, err := manager.Acquire("worker", "sandbox")
	require.ErrorIs(t, err, errInjected)
	manager = restartManager(t, manager, backend)
	_, err = manager.Acquire("worker", "sandbox")
	require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	assert.Equal(t, 1, backend.creates)
	assert.Len(t, backend.namespaces, 1)
}

func TestRecoverReleasedSandbox(t *testing.T) {
	manager, backend := setupManager(t)
	member, err := manager.Acquire("worker", "sandbox")
	require.NoError(t, err)
	require.NoError(t, manager.Release(member))
	manager = restartManager(t, manager, backend)
	require.NoError(t, manager.Recover([]Membership{member}))
	assert.Len(t, backend.namespaces, 1)
	assert.Empty(t, manager.groups["worker"].Members)
	require.ErrorIs(t, manager.Validate(member), errdefs.ErrFailedPrecondition)
}

func TestRecoverInterruptedRelease(t *testing.T) {
	manager, backend := setupManager(t)
	member, err := manager.Acquire("worker", "sandbox")
	require.NoError(t, err)
	delete(backend.namespaces, member.Path)
	manager = restartManager(t, manager, backend)
	require.NoError(t, manager.Recover([]Membership{member}))
	assert.Len(t, backend.namespaces, 1)
	assert.Empty(t, manager.groups["worker"].Members)
}

func TestReleaseRetries(t *testing.T) {
	manager, backend := setupManager(t)
	member, err := manager.Acquire("worker", "sandbox")
	require.NoError(t, err)
	backend.removeErr = errInjected
	require.ErrorIs(t, manager.Release(member), errInjected)
	require.NoError(t, manager.Validate(member))
	backend.removeErr = nil
	manager.write = func(string, []byte) error { return errInjected }
	require.ErrorIs(t, manager.Release(member), errInjected)
	assert.NotContains(t, backend.namespaces, member.Path)
	assert.Contains(t, manager.groups["worker"].Members, member.SandboxID)
	manager = restartManager(t, manager, backend)
	require.NoError(t, manager.Release(member))
	require.NoError(t, manager.Release(member))
	assert.Contains(t, backend.namespaces, member.CanonicalPath)
}

func TestReplacedNamespaceFailsClosed(t *testing.T) {
	t.Run("alias", func(t *testing.T) {
		manager, backend := setupManager(t)
		member, err := manager.Acquire("worker", "sandbox")
		require.NoError(t, err)
		replacement := identity{Device: 4, Inode: 999}
		backend.namespaces[member.Path] = replacement
		require.ErrorIs(t, manager.Validate(member), errdefs.ErrFailedPrecondition)
		require.ErrorIs(t, manager.Release(member), errdefs.ErrFailedPrecondition)
		require.ErrorIs(t, manager.Recover(nil), errdefs.ErrFailedPrecondition)
		_, err = manager.Acquire("worker", "sandbox")
		require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
		assert.Equal(t, replacement, backend.namespaces[member.Path])
	})
	t.Run("canonical", func(t *testing.T) {
		manager, backend := setupManager(t)
		member, err := manager.Acquire("worker", "sandbox")
		require.NoError(t, err)
		replacement := identity{Device: 4, Inode: 999}
		backend.namespaces[member.CanonicalPath] = replacement
		require.ErrorIs(t, manager.Validate(member), errdefs.ErrFailedPrecondition)
		_, err = manager.Acquire("worker", "sandbox-new")
		require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
		require.NoError(t, manager.Release(member))
		require.ErrorIs(t, manager.DeleteGroup("worker"), errdefs.ErrFailedPrecondition)
		assert.Equal(t, replacement, backend.namespaces[member.CanonicalPath])
	})
}

func TestRecoveredMembershipMustMatch(t *testing.T) {
	manager, backend := setupManager(t)
	member, err := manager.Acquire("worker", "sandbox")
	require.NoError(t, err)
	modified := member
	modified.Generation = strings.Repeat("f", 32)
	require.ErrorIs(t, manager.Recover([]Membership{modified}), errdefs.ErrFailedPrecondition)
	require.ErrorIs(t, manager.Release(modified), errdefs.ErrFailedPrecondition)
	assert.Contains(t, backend.namespaces, member.Path)
	require.NoError(t, manager.Validate(member))
}

func TestBootChangeDoesNotAdoptOrUnmountOldPaths(t *testing.T) {
	manager, backend := setupManager(t)
	member, err := manager.Acquire("worker", "sandbox")
	require.NoError(t, err)
	manager, err = newManager(manager.rootDir, manager.stateDir, "next-boot", backend)
	require.NoError(t, err)
	require.ErrorIs(t, manager.Validate(member), errdefs.ErrFailedPrecondition)
	_, err = manager.Acquire("worker", "new-sandbox")
	require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	require.NoError(t, manager.Recover(nil))
	assert.Contains(t, backend.namespaces, member.Path)
	assert.Contains(t, backend.namespaces, member.CanonicalPath)
	later, err := manager.Acquire("worker", "new-sandbox")
	require.NoError(t, err)
	assert.NotEqual(t, member.Generation, later.Generation)
	assert.NotEqual(t, member.Inode, later.Inode)
	assert.Equal(t, "next-boot", later.BootID)
}

func TestRecoverAfterBootChangeWithMissingMounts(t *testing.T) {
	manager, backend := setupManager(t)
	previous, err := manager.Acquire("worker", "previous-sandbox")
	require.NoError(t, err)
	backend.namespaces = map[string]identity{}
	manager, err = newManager(manager.rootDir, manager.stateDir, "next-boot", backend)
	require.NoError(t, err)
	require.NoError(t, manager.Recover([]Membership{previous}))
	require.ErrorIs(t, manager.Validate(previous), errdefs.ErrFailedPrecondition)
	assert.Empty(t, backend.namespaces)
	current, err := manager.Acquire("worker", "current-sandbox")
	require.NoError(t, err)
	assert.NotEqual(t, previous.Generation, current.Generation)
	manager = restartManager(t, manager, backend)
	require.NoError(t, manager.Recover([]Membership{previous, current}))
	require.NoError(t, manager.Validate(current))
	assert.NotContains(t, backend.namespaces, previous.Path)
	assert.Len(t, backend.namespaces, 2)
}

func TestInvalidGroupAndSandboxNames(t *testing.T) {
	manager, backend := setupManager(t)
	for _, group := range []string{"", "../escape", "UPPER", "a..b", "-worker", "worker-", strings.Repeat("x", 64)} {
		_, err := manager.Acquire(group, "sandbox")
		assert.ErrorIs(t, err, errdefs.ErrInvalidArgument, group)
	}
	_, err := manager.Acquire("worker", "../sandbox")
	assert.ErrorIs(t, err, errdefs.ErrInvalidArgument)
	assert.Empty(t, backend.namespaces)
	_, err = manager.Acquire(strings.Repeat("x", 63), "sandbox")
	require.NoError(t, err)
}

func TestManifestPathsAreValidated(t *testing.T) {
	manager, backend := setupManager(t)
	_, err := manager.Acquire("worker", "sandbox")
	require.NoError(t, err)
	manager.groups["worker"].CanonicalPath = "/outside/namespace"
	data, err := json.Marshal(manifest{Version: 1, Groups: manager.groups})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(manager.rootDir, "groups.json"), data, 0600))
	_, err = newManager(manager.rootDir, manager.stateDir, manager.bootID, backend)
	require.Error(t, err)
	assert.Len(t, backend.namespaces, 2)
}

func TestDeleteGroupRequiresNoMembers(t *testing.T) {
	manager, backend := setupManager(t)
	member, err := manager.Acquire("worker", "sandbox")
	require.NoError(t, err)
	require.ErrorIs(t, manager.DeleteGroup("worker"), errdefs.ErrFailedPrecondition)
	require.NoError(t, manager.Release(member))
	require.NoError(t, manager.DeleteGroup("worker"))
	require.NoError(t, manager.DeleteGroup("worker"))
	assert.Empty(t, backend.namespaces)
	manager = restartManager(t, manager, backend)
	assert.Empty(t, manager.groups)
}
