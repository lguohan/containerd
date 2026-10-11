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

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	"github.com/containerd/containerd/v2/internal/cri/sharednetns"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSharedSandboxMetricsRequireValidMembership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unverified-namespace")
	contents := []byte("replacement must remain intact")
	require.NoError(t, os.WriteFile(path, contents, 0600))
	service := newTestCRIService()
	service.sharedNetNS = &sharednetns.Manager{}
	sandbox := sandboxstore.NewSandbox(sandboxstore.Metadata{
		ID: "shared-sandbox", Config: sharedNetNSTestConfig(), RuntimeHandler: criconfig.RuntimeSharedNetNS,
		NetNSPath:   path,
		SharedNetNS: &sharednetns.Membership{Group: "shared-runtime", SandboxID: "shared-sandbox", Path: path},
	}, sandboxstore.Status{State: sandboxstore.StateReady})

	stats, err := service.podSandboxStats(context.Background(), sandbox)
	require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	assert.Nil(t, stats)
	metrics, err := service.collectPodSandboxMetrics(context.Background(), sandbox, nil)
	require.ErrorIs(t, err, errdefs.ErrFailedPrecondition)
	assert.Nil(t, metrics)
	preserved, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, contents, preserved)

	sandbox.SharedNetNS = nil
	sandbox.RuntimeHandler = "runc"
	sandbox.NetNSPath = ""
	metrics, err = service.collectPodSandboxMetrics(context.Background(), sandbox, nil)
	require.NoError(t, err)
	assert.Equal(t, sandbox.ID, metrics.PodSandboxId)
	_, err = service.podSandboxStats(context.Background(), sandbox)
	require.ErrorIs(t, err, errdefs.ErrNotImplemented)
}
