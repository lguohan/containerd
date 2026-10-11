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

package sandbox

import (
	"encoding/json"
	"testing"

	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestSharedRuntimeMetadataSurvivesCheckpointAndConfigChange(t *testing.T) {
	originalRuntime := criconfig.Runtime{
		Type: "io.containerd.runc.v2", Path: "/custom/containerd-shim-runc-v2",
		PodAnnotations: []string{"example.org/pod-*"}, ContainerAnnotations: []string{"example.org/container-*"},
		Options:                      map[string]any{"SystemdCgroup": true, "IoUid": int64(1234), "IoGid": int64(5678), "BinaryName": "/custom/runc"},
		PrivilegedWithoutHostDevices: true, PrivilegedWithoutHostDevicesAllDevicesAllowed: true,
		CgroupWritable: true, BaseRuntimeSpec: "/custom/spec.json",
		NetworkPluginConfDir: "/custom/cni", NetworkPluginMaxConfNum: 2,
		Snapshotter: "native", Sandboxer: string(criconfig.ModePodSandbox),
		DisablePauseImagePull: true, IOType: criconfig.IOTypeFifo,
	}
	savedConfig, err := toml.Marshal(originalRuntime)
	require.NoError(t, err)
	metadata := &Metadata{
		ID: "shared-sandbox", RuntimeHandler: criconfig.RuntimeSharedNetNS,
		ExecutionRuntimeHandler: "old-runc", ExecutionRuntimeConfig: string(savedConfig),
		ExecutionRuntimeFeatures: &runtime.RuntimeHandlerFeatures{RecursiveReadOnlyMounts: true},
	}
	checkpoint, err := json.Marshal(metadata)
	require.NoError(t, err)
	var restored Metadata
	require.NoError(t, json.Unmarshal(checkpoint, &restored))
	assert.Equal(t, criconfig.RuntimeSharedNetNS, restored.RuntimeHandler)
	assert.Equal(t, "old-runc", restored.ExecutionRuntimeHandler)
	assert.True(t, restored.ExecutionRuntimeFeatures.GetRecursiveReadOnlyMounts())

	config := &criconfig.Config{RuntimeConfig: criconfig.RuntimeConfig{ContainerdConfig: criconfig.ContainerdConfig{
		DefaultRuntimeName: "new-runtime",
		Runtimes:           map[string]criconfig.Runtime{"new-runtime": {Type: "io.containerd.other.v1", Snapshotter: "different"}},
	}}}
	resolvedRuntime, err := restored.GetRuntime(config)
	require.NoError(t, err)
	assert.Equal(t, originalRuntime, resolvedRuntime)
	assert.IsType(t, int64(0), resolvedRuntime.Options["IoUid"])
	assert.IsType(t, int64(0), resolvedRuntime.Options["IoGid"])
	generatedOptions, err := criconfig.GenerateRuntimeOptions(resolvedRuntime)
	require.NoError(t, err)
	options, valid := generatedOptions.(*runcoptions.Options)
	require.True(t, valid)
	assert.Equal(t, uint32(1234), options.IoUid)
	assert.Equal(t, uint32(5678), options.IoGid)
	assert.Equal(t, "/custom/runc", options.BinaryName)
	assert.True(t, options.SystemdCgroup)
}

func TestSharedRuntimeMetadataRequiresSavedExecutionRuntime(t *testing.T) {
	for _, test := range []struct {
		name      string
		handler   string
		config    string
		wantError string
	}{
		{name: "missing handler", config: `runtime_type = "io.containerd.runc.v2"`, wantError: "has no saved execution runtime"},
		{name: "recursive handler", handler: criconfig.RuntimeSharedNetNS, config: `runtime_type = "io.containerd.runc.v2"`, wantError: "has no saved execution runtime"},
		{name: "missing config", handler: "old-runc", wantError: "has no saved execution runtime"},
		{name: "malformed config", handler: "old-runc", config: "[invalid", wantError: "decode saved execution runtime"},
		{name: "empty TOML", handler: "old-runc", config: " ", wantError: "incompatible saved execution runtime"},
		{name: "missing runtime type", handler: "old-runc", config: `sandboxer = "podsandbox"`, wantError: "incompatible saved execution runtime"},
		{name: "unsupported runtime type", handler: "old-runc", config: `runtime_type = "io.containerd.kata.v2"`, wantError: "incompatible saved execution runtime"},
		{name: "unsupported sandboxer", handler: "old-runc", config: "runtime_type = \"io.containerd.runc.v2\"\nsandboxer = \"shim\"\n", wantError: "incompatible saved execution runtime"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := &Metadata{
				ID: "broken-sandbox", RuntimeHandler: criconfig.RuntimeSharedNetNS,
				ExecutionRuntimeHandler: test.handler, ExecutionRuntimeConfig: test.config,
			}
			config := &criconfig.Config{RuntimeConfig: criconfig.RuntimeConfig{ContainerdConfig: criconfig.ContainerdConfig{
				DefaultRuntimeName: "new-runc", Runtimes: map[string]criconfig.Runtime{"new-runc": {Type: "io.containerd.runc.v2"}},
			}}}
			resolvedRuntime, err := metadata.GetRuntime(config)
			require.ErrorContains(t, err, test.wantError)
			assert.Equal(t, criconfig.Runtime{}, resolvedRuntime)
		})
	}
}

func TestOrdinaryRuntimeMetadataUsesCurrentConfiguration(t *testing.T) {
	defaultRuntime := criconfig.Runtime{Type: "io.containerd.runc.v2", Snapshotter: "new-default"}
	explicitRuntime := criconfig.Runtime{Type: "io.containerd.other.v1", Snapshotter: "other"}
	config := &criconfig.Config{RuntimeConfig: criconfig.RuntimeConfig{ContainerdConfig: criconfig.ContainerdConfig{
		DefaultRuntimeName: "new-runc",
		Runtimes:           map[string]criconfig.Runtime{"new-runc": defaultRuntime, "other": explicitRuntime},
	}}}
	for _, test := range []struct {
		handler     string
		wantRuntime criconfig.Runtime
	}{
		{wantRuntime: defaultRuntime},
		{handler: "other", wantRuntime: explicitRuntime},
	} {
		metadata := &Metadata{RuntimeHandler: test.handler, ExecutionRuntimeHandler: "old-runc", ExecutionRuntimeConfig: "invalid TOML"}
		resolvedRuntime, err := metadata.GetRuntime(config)
		require.NoError(t, err)
		assert.Equal(t, test.wantRuntime, resolvedRuntime)
	}
	_, err := (&Metadata{RuntimeHandler: "missing"}).GetRuntime(config)
	require.ErrorContains(t, err, `no runtime for "missing" is configured`)
}
