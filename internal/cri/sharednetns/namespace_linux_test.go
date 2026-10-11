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
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/containerd/v2/pkg/testutil"
	"github.com/containerd/errdefs"
	cnins "github.com/containernetworking/plugins/pkg/ns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

func TestRealNamespaceRetention(t *testing.T) {
	testutil.RequiresRoot(t)
	rootDir, stateDir := t.TempDir(), t.TempDir()
	manager, err := New(rootDir, stateDir)
	require.NoError(t, err)
	var members []Membership
	t.Cleanup(func() {
		for _, member := range members {
			assert.NoError(t, manager.Release(member))
		}
		assert.NoError(t, manager.DeleteGroup("worker"))
		assert.NoError(t, manager.DeleteGroup("other"))
	})
	first, err := manager.Acquire("worker", "sandbox-one")
	require.NoError(t, err)
	members = append(members, first)
	second, err := manager.Acquire("worker", "sandbox-two")
	require.NoError(t, err)
	members = append(members, second)
	other, err := manager.Acquire("other", "sandbox-other")
	require.NoError(t, err)
	members = append(members, other)
	assert.Equal(t, first.Inode, second.Inode)
	assert.NotEqual(t, first.Inode, other.Inode)
	require.NoError(t, netns.LoadNetNS(first.Path).Do(func(cnins.NetNS) error {
		links, err := netlink.LinkList()
		if err != nil {
			return err
		}
		require.Len(t, links, 1)
		assert.Equal(t, "lo", links[0].Attrs().Name)
		assert.Zero(t, links[0].Attrs().Flags&net.FlagUp)
		return netlink.LinkSetMTU(links[0], 1234)
	}))
	require.NoError(t, manager.Release(first))
	require.NoError(t, manager.Validate(second))
	manager, err = New(rootDir, stateDir)
	require.NoError(t, err)
	require.NoError(t, manager.Recover([]Membership{second, other}))
	require.NoError(t, manager.Release(second))
	later, err := manager.Acquire("worker", "sandbox-later")
	require.NoError(t, err)
	members = append(members, later)
	assert.Equal(t, first.Inode, later.Inode)
	require.NoError(t, netns.LoadNetNS(later.Path).Do(func(cnins.NetNS) error {
		loopback, err := netlink.LinkByName("lo")
		if err != nil {
			return err
		}
		assert.Equal(t, 1234, loopback.Attrs().MTU)
		return nil
	}))
}

func TestLinuxRejectsNonNamespaceAndSymlink(t *testing.T) {
	backend := linuxBackend{}
	regular := filepath.Join(t.TempDir(), "regular")
	require.NoError(t, os.WriteFile(regular, nil, 0600))
	_, err := backend.Inspect(regular)
	require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	require.Error(t, backend.Remove(regular, identity{}))
	_, err = os.Stat(regular)
	require.NoError(t, err)
	symlink := filepath.Join(t.TempDir(), "symlink")
	require.NoError(t, os.Symlink("/proc/self/ns/net", symlink))
	_, err = backend.Inspect(symlink)
	require.Error(t, err)
	require.Error(t, backend.Remove(symlink, identity{}))
	_, err = os.Lstat(symlink)
	require.NoError(t, err)
}
