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

package config

import (
	"context"
	"runtime"
	"testing"

	"github.com/containerd/containerd/v2/internal/cri/annotations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	criruntime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestResolveSharedNetNSRuntime(t *testing.T) {
	configuredRuntime := Runtime{
		Type:                         "io.containerd.runc.v2",
		Path:                         "/custom/containerd-shim-runc-v2",
		PodAnnotations:               []string{"example.org/pod-*"},
		ContainerAnnotations:         []string{"example.org/container-*"},
		Options:                      map[string]any{"SystemdCgroup": true, "BinaryName": "/custom/runc"},
		PrivilegedWithoutHostDevices: true,
		PrivilegedWithoutHostDevicesAllDevicesAllowed: true,
		CgroupWritable:          true,
		BaseRuntimeSpec:         "/custom/spec.json",
		NetworkPluginConfDir:    "/custom/cni",
		NetworkPluginMaxConfNum: 2,
		Snapshotter:             "native",
		Sandboxer:               string(ModePodSandbox),
		DisablePauseImagePull:   true,
		IOType:                  IOTypeFifo,
	}
	config := Config{RuntimeConfig: RuntimeConfig{ContainerdConfig: ContainerdConfig{
		DefaultRuntimeName: "custom-runc",
		Runtimes:           map[string]Runtime{"custom-runc": configuredRuntime},
	}}}

	handler, resolvedRuntime, err := config.ResolveSandboxRuntime(nil, RuntimeSharedNetNS)
	if runtime.GOOS != "linux" {
		require.ErrorContains(t, err, "only supported on Linux")
		return
	}
	require.NoError(t, err)
	assert.Equal(t, "custom-runc", handler)
	assert.Equal(t, configuredRuntime, resolvedRuntime)
	assert.NotContains(t, config.Runtimes, RuntimeSharedNetNS)
	legacyRuntime, err := config.GetSandboxRuntime(nil, RuntimeSharedNetNS)
	require.NoError(t, err)
	assert.Equal(t, configuredRuntime, legacyRuntime)
}

func TestResolveSharedNetNSRuntimeRejectsUnsupportedConfig(t *testing.T) {
	for _, test := range []struct {
		name        string
		defaultName string
		runtimes    map[string]Runtime
		wantError   string
	}{
		{
			name: "reserved name", defaultName: "runc",
			runtimes:  map[string]Runtime{"runc": {Type: "io.containerd.runc.v2"}, RuntimeSharedNetNS: {}},
			wantError: "reserved for shared network namespaces",
		},
		{
			name: "recursive default", defaultName: RuntimeSharedNetNS,
			wantError: "cannot be \"shared-netns\"",
		},
		{
			name: "missing default", defaultName: "missing",
			wantError: "no runtime for \"missing\" is configured",
		},
		{
			name: "unsupported runtime", defaultName: "kata",
			runtimes:  map[string]Runtime{"kata": {Type: "io.containerd.kata.v2"}},
			wantError: "requires default runtime type io.containerd.runc.v2",
		},
		{
			name: "unsupported sandboxer", defaultName: "runc",
			runtimes:  map[string]Runtime{"runc": {Type: "io.containerd.runc.v2", Sandboxer: "shim"}},
			wantError: "requires the podsandbox controller",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{RuntimeConfig: RuntimeConfig{ContainerdConfig: ContainerdConfig{
				DefaultRuntimeName: test.defaultName,
				Runtimes:           test.runtimes,
			}}}
			_, _, err := config.ResolveSandboxRuntime(nil, RuntimeSharedNetNS)
			if runtime.GOOS != "linux" && (test.name == "unsupported runtime" || test.name == "unsupported sandboxer") {
				require.ErrorContains(t, err, "only supported on Linux")
				return
			}
			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestResolveSandboxRuntimePreservesSelection(t *testing.T) {
	config := Config{RuntimeConfig: RuntimeConfig{ContainerdConfig: ContainerdConfig{
		DefaultRuntimeName: "runc",
		Runtimes: map[string]Runtime{
			"runc":           {Type: "io.containerd.runc.v2"},
			"other":          {Type: "io.containerd.other.v1"},
			RuntimeUntrusted: {Type: "io.containerd.untrusted.v1"},
		},
	}}}
	for _, test := range []struct {
		name        string
		requested   string
		podConfig   *criruntime.PodSandboxConfig
		wantHandler string
		wantError   string
	}{
		{name: "implicit default", wantHandler: "runc"},
		{name: "explicit handler", requested: "other", wantHandler: "other"},
		{name: "unknown handler", requested: "missing", wantError: "no runtime for \"missing\" is configured"},
		{
			name: "untrusted annotation", wantHandler: RuntimeUntrusted,
			podConfig: &criruntime.PodSandboxConfig{Annotations: map[string]string{annotations.UntrustedWorkload: "true"}},
		},
		{
			name: "shared untrusted denied", requested: RuntimeSharedNetNS,
			podConfig: &criruntime.PodSandboxConfig{Annotations: map[string]string{annotations.UntrustedWorkload: "true"}},
			wantError: "untrusted workload with explicit runtime handler is not allowed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, resolvedRuntime, err := config.ResolveSandboxRuntime(test.podConfig, test.requested)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantHandler, handler)
			assert.Equal(t, config.Runtimes[test.wantHandler], resolvedRuntime)
		})
	}
}

func TestValidateReservedSharedNetNSRuntime(t *testing.T) {
	for _, test := range []struct {
		name      string
		config    ContainerdConfig
		wantError string
	}{
		{
			name:      "reserved runtime name",
			config:    ContainerdConfig{DefaultRuntimeName: "runc", Runtimes: map[string]Runtime{"runc": {}, RuntimeSharedNetNS: {}}},
			wantError: "reserved for shared network namespaces",
		},
		{
			name:      "recursive default",
			config:    ContainerdConfig{DefaultRuntimeName: RuntimeSharedNetNS, Runtimes: map[string]Runtime{RuntimeSharedNetNS: {}}},
			wantError: "cannot be \"shared-netns\"",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ValidateRuntimeConfig(context.Background(), &RuntimeConfig{ContainerdConfig: test.config})
			require.ErrorContains(t, err, test.wantError)
		})
	}
}
