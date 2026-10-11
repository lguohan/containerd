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
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	coresandbox "github.com/containerd/containerd/v2/core/sandbox"
	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	podsandboxtypes "github.com/containerd/containerd/v2/internal/cri/server/podsandbox/types"
	"github.com/containerd/containerd/v2/internal/cri/sharednetns"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func sharedNetNSTestConfig() *runtime.PodSandboxConfig {
	return &runtime.PodSandboxConfig{
		Metadata: &runtime.PodSandboxMetadata{Name: "pod", Namespace: "first-namespace"},
		Labels:   map[string]string{sharedNetNSGroupLabel: "shared-runtime"},
		Linux: &runtime.LinuxPodSandboxConfig{SecurityContext: &runtime.LinuxSandboxSecurityContext{
			NamespaceOptions: &runtime.NamespaceOption{Network: runtime.NamespaceMode_POD},
		}},
	}
}

func TestSharedNetNSGroupValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		modify    func(*runtime.PodSandboxConfig)
		wantError string
	}{
		{
			name: "missing label", modify: func(config *runtime.PodSandboxConfig) { config.Labels = nil },
			wantError: "requires label",
		},
		{
			name: "empty label", modify: func(config *runtime.PodSandboxConfig) { config.Labels[sharedNetNSGroupLabel] = "" },
			wantError: "requires label",
		},
		{
			name: "long label", modify: func(config *runtime.PodSandboxConfig) { config.Labels[sharedNetNSGroupLabel] = strings.Repeat("a", 64) },
			wantError: "1 to 63 characters",
		},
		{
			name: "missing Linux configuration", modify: func(config *runtime.PodSandboxConfig) { config.Linux = nil },
			wantError: "requires a Linux sandbox",
		},
		{
			name: "Windows configuration", modify: func(config *runtime.PodSandboxConfig) { config.Windows = &runtime.WindowsPodSandboxConfig{} },
			wantError: "requires a Linux sandbox",
		},
		{
			name: "host network", modify: func(config *runtime.PodSandboxConfig) {
				config.Linux.SecurityContext.NamespaceOptions.Network = runtime.NamespaceMode_NODE
			},
			wantError: "requires private Pod networking",
		},
		{
			name: "target network", modify: func(config *runtime.PodSandboxConfig) {
				config.Linux.SecurityContext.NamespaceOptions.Network = runtime.NamespaceMode_TARGET
			},
			wantError: "requires private Pod networking",
		},
		{
			name: "container network", modify: func(config *runtime.PodSandboxConfig) {
				config.Linux.SecurityContext.NamespaceOptions.Network = runtime.NamespaceMode_CONTAINER
			},
			wantError: "requires private Pod networking",
		},
		{
			name: "Pod user namespace", modify: func(config *runtime.PodSandboxConfig) {
				config.Linux.SecurityContext.NamespaceOptions.UsernsOptions = &runtime.UserNamespace{Mode: runtime.NamespaceMode_POD}
			},
			wantError: "does not support Pod user namespaces",
		},
		{
			name: "port mappings", modify: func(config *runtime.PodSandboxConfig) {
				config.PortMappings = []*runtime.PortMapping{{ContainerPort: 8080, HostPort: 18080}}
			},
			wantError: "does not configure port mappings",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := sharedNetNSTestConfig()
			test.modify(config)
			group, err := sharedNetNSGroup(config, criconfig.RuntimeSharedNetNS)
			if goruntime.GOOS != "linux" {
				require.ErrorContains(t, err, "requires a Linux sandbox")
			} else {
				require.ErrorContains(t, err, test.wantError)
			}
			assert.Empty(t, group)
		})
	}

	group, err := sharedNetNSGroup(nil, criconfig.RuntimeSharedNetNS)
	require.ErrorContains(t, err, "requires a Linux sandbox")
	assert.Empty(t, group)
}

func TestSharedNetNSGroupNames(t *testing.T) {
	for _, test := range []struct {
		group string
		valid bool
	}{
		{group: "a", valid: true},
		{group: "runtime-1", valid: true},
		{group: "runtime.example", valid: true},
		{group: strings.Repeat("a", 63), valid: true},
		{group: "Uppercase"},
		{group: "has_underscore"},
		{group: "has space"},
		{group: "../escape"},
		{group: "name/escape"},
		{group: "-prefix"},
		{group: "suffix-"},
		{group: "empty..component"},
		{group: "."},
	} {
		t.Run(test.group, func(t *testing.T) {
			config := sharedNetNSTestConfig()
			config.Labels[sharedNetNSGroupLabel] = test.group
			group, err := sharedNetNSGroup(config, criconfig.RuntimeSharedNetNS)
			if goruntime.GOOS != "linux" {
				require.ErrorContains(t, err, "requires a Linux sandbox")
			} else if test.valid {
				require.NoError(t, err)
				assert.Equal(t, test.group, group)
			} else {
				require.ErrorContains(t, err, "invalid shared-netns RuntimeClass name")
				assert.Empty(t, group)
			}
		})
	}
}

func TestSharedNetNSGroupIgnoresOrdinaryHandlers(t *testing.T) {
	config := sharedNetNSTestConfig()
	config.Labels[sharedNetNSGroupLabel] = "invalid/group"
	config.Linux = nil
	config.PortMappings = []*runtime.PortMapping{{HostPort: 8080}}
	for _, handler := range []string{"", "runc", "another-runtime"} {
		group, err := sharedNetNSGroup(config, handler)
		require.NoError(t, err)
		assert.Empty(t, group)
	}
}

func TestSharedNetNSGroupCrossesAPINamespaces(t *testing.T) {
	firstConfig := sharedNetNSTestConfig()
	secondConfig := sharedNetNSTestConfig()
	secondConfig.Metadata.Namespace = "second-namespace"
	secondConfig.Linux.SecurityContext.NamespaceOptions.UsernsOptions = &runtime.UserNamespace{Mode: runtime.NamespaceMode_NODE}
	firstGroup, firstError := sharedNetNSGroup(firstConfig, criconfig.RuntimeSharedNetNS)
	secondGroup, secondError := sharedNetNSGroup(secondConfig, criconfig.RuntimeSharedNetNS)
	if goruntime.GOOS != "linux" {
		require.ErrorContains(t, firstError, "requires a Linux sandbox")
		require.ErrorContains(t, secondError, "requires a Linux sandbox")
		return
	}
	require.NoError(t, firstError)
	require.NoError(t, secondError)
	assert.Equal(t, "shared-runtime", firstGroup)
	assert.Equal(t, firstGroup, secondGroup)
}

func TestSharedNetNSSkipsCNINetworking(t *testing.T) {
	service := &criService{}
	service.config.UseInternalLoopback = true
	sandbox := sandboxstore.Sandbox{Metadata: sandboxstore.Metadata{
		ID: "shared-sandbox", Config: sharedNetNSTestConfig(), RuntimeHandler: criconfig.RuntimeSharedNetNS,
		NetNSPath:   "/not-a-real-network-namespace",
		SharedNetNS: &sharednetns.Membership{Group: "shared-runtime", SandboxID: "shared-sandbox"},
	}}
	require.NoError(t, service.setupPodNetwork(context.Background(), &sandbox))
	require.NoError(t, service.teardownPodNetwork(context.Background(), sandbox))
	assert.Empty(t, sandbox.IP)
	assert.Empty(t, sandbox.AdditionalIPs)
	assert.Nil(t, sandbox.CNIResult)
	assert.Equal(t, "/not-a-real-network-namespace", sandbox.NetNSPath)

	sandbox.SharedNetNS = nil
	sandbox.RuntimeHandler = "runc"
	require.ErrorContains(t, service.setupPodNetwork(context.Background(), &sandbox), "cni config not initialized")
	require.ErrorContains(t, service.teardownPodNetwork(context.Background(), sandbox), "cni config not initialized")
}

func TestDeletedSharedSandboxStatusPreservesReplacementFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replaced-alias")
	contents := []byte("replacement must remain intact")
	require.NoError(t, os.WriteFile(path, contents, 0600))
	sandbox := sandboxstore.NewSandbox(sandboxstore.Metadata{
		ID: "shared-sandbox", Config: sharedNetNSTestConfig(), RuntimeHandler: criconfig.RuntimeSharedNetNS,
		NetNSPath:   path,
		SharedNetNS: &sharednetns.Membership{Group: "shared-runtime", SandboxID: "shared-sandbox", Path: path},
	}, sandboxstore.Status{State: sandboxstore.StateNotReady})
	sandbox.NetNS = netns.LoadNetNS(path)
	info, err := toDeletedCRISandboxInfo(sandbox)
	require.NoError(t, err)
	assert.Contains(t, info, "info")
	preserved, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, contents, preserved)
}

type sharedNetNSStatusService struct {
	fakeSandboxService
	state sandboxstore.State
}

func (service *sharedNetNSStatusService) SandboxStatus(context.Context, string, string, bool) (coresandbox.ControllerStatus, error) {
	return coresandbox.ControllerStatus{
		State: service.state.String(), CreatedAt: time.Unix(100, 0),
		Info: map[string]string{"info": `{"pid":42,"netNamespaceClosed":false}`},
	}, nil
}

func TestSharedSandboxStatusReportsUnusableNamespace(t *testing.T) {
	for _, state := range []sandboxstore.State{sandboxstore.StateReady, sandboxstore.StateNotReady} {
		t.Run(state.String(), func(t *testing.T) {
			service := newTestCRIService()
			service.sharedNetNS = &sharednetns.Manager{}
			service.sandboxService = &sharedNetNSStatusService{state: state}
			sandbox := sandboxstore.NewSandbox(sandboxstore.Metadata{
				ID: "shared-sandbox", Config: sharedNetNSTestConfig(), RuntimeHandler: criconfig.RuntimeSharedNetNS,
				SharedNetNS: &sharednetns.Membership{Group: "shared-runtime", SandboxID: "shared-sandbox"},
			}, sandboxstore.Status{State: state})
			require.NoError(t, service.sandboxStore.Add(sandbox))
			for _, verbose := range []bool{false, true} {
				response, err := service.PodSandboxStatus(context.Background(), &runtime.PodSandboxStatusRequest{PodSandboxId: sandbox.ID, Verbose: verbose})
				require.NoError(t, err)
				assert.Equal(t, runtime.PodSandboxState_SANDBOX_NOTREADY, response.Status.State)
				assert.Contains(t, response.Info["sharedNetNSError"], "unknown shared namespace membership")
				var info podsandboxtypes.SandboxInfo
				require.NoError(t, json.Unmarshal([]byte(response.Info["info"]), &info))
				assert.True(t, info.NetNSClosed)
				assert.Equal(t, uint32(42), info.Pid)
			}
		})
	}
}

func TestSetSandboxNetworkClosedPreservesInfo(t *testing.T) {
	for _, closed := range []bool{false, true} {
		info := map[string]string{"info": `{"pid":42,"netNamespaceClosed":false,"extension":{"number":9007199254740993}}`}
		require.NoError(t, setSandboxNetworkClosed(info, closed))
		var decoded map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(info["info"]), &decoded))
		assert.JSONEq(t, `{"number":9007199254740993}`, string(decoded["extension"]))
		assert.Equal(t, "42", string(decoded["pid"]))
		var decodedClosed bool
		require.NoError(t, json.Unmarshal(decoded["netNamespaceClosed"], &decodedClosed))
		assert.Equal(t, closed, decodedClosed)
	}
}
