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

package podsandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	"github.com/containerd/containerd/v2/internal/cri/server/podsandbox/types"
	"github.com/containerd/containerd/v2/internal/cri/sharednetns"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestSharedSandboxStatusPreservesReplacementFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replaced-alias")
	contents := []byte("replacement must remain intact")
	require.NoError(t, os.WriteFile(path, contents, 0600))
	sandbox := types.NewPodSandbox("shared-sandbox", sandboxstore.Status{State: sandboxstore.StateNotReady})
	sandbox.Metadata = sandboxstore.Metadata{
		ID: "shared-sandbox", RuntimeHandler: criconfig.RuntimeSharedNetNS,
		Config:      &runtime.PodSandboxConfig{Linux: &runtime.LinuxPodSandboxConfig{}},
		NetNSPath:   path,
		SharedNetNS: &sharednetns.Membership{Group: "shared-runtime", SandboxID: "shared-sandbox", Path: path},
	}
	info, err := toCRISandboxInfo(context.Background(), sandbox)
	require.NoError(t, err)
	assert.Contains(t, info, "info")
	preserved, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, contents, preserved)
}
