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
	"runtime"
	"testing"

	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	criruntime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestRegisterSharedNetNSRuntimeHandler(t *testing.T) {
	for _, test := range []struct {
		name           string
		defaultRuntime criconfig.Runtime
		features       *criruntime.RuntimeHandlerFeatures
		wantHandler    bool
	}{
		{
			name: "default features", defaultRuntime: criconfig.Runtime{Type: "io.containerd.runc.v2"},
			features: &criruntime.RuntimeHandlerFeatures{RecursiveReadOnlyMounts: true, UserNamespaces: true}, wantHandler: true,
		},
		{name: "unknown features", defaultRuntime: criconfig.Runtime{Type: "io.containerd.runc.v2"}, wantHandler: true},
		{name: "unsupported runtime", defaultRuntime: criconfig.Runtime{Type: "io.containerd.kata.v2"}},
		{name: "unsupported sandboxer", defaultRuntime: criconfig.Runtime{Type: "io.containerd.runc.v2", Sandboxer: "shim"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &criService{
				config: criconfig.Config{RuntimeConfig: criconfig.RuntimeConfig{ContainerdConfig: criconfig.ContainerdConfig{
					DefaultRuntimeName: "custom-runc",
					Runtimes:           map[string]criconfig.Runtime{"custom-runc": test.defaultRuntime},
				}}},
				runtimeHandlers: map[string]*criruntime.RuntimeHandler{
					"custom-runc": {Name: "custom-runc", Features: test.features},
				},
			}
			service.registerSharedNetNSRuntimeHandler()
			if !test.wantHandler || runtime.GOOS != "linux" {
				assert.NotContains(t, service.runtimeHandlers, criconfig.RuntimeSharedNetNS)
				return
			}
			sharedHandler := service.runtimeHandlers[criconfig.RuntimeSharedNetNS]
			require.NotNil(t, sharedHandler)
			assert.Equal(t, criconfig.RuntimeSharedNetNS, sharedHandler.Name)
			assert.Equal(t, test.features.GetRecursiveReadOnlyMounts(), sharedHandler.Features.GetRecursiveReadOnlyMounts())
			assert.False(t, sharedHandler.Features.GetUserNamespaces())
			assert.Same(t, test.features, service.runtimeHandlers["custom-runc"].Features)
		})
	}
}

func TestSharedSandboxRuntimeFeaturesSurviveDefaultChange(t *testing.T) {
	persistedFeatures := &criruntime.RuntimeHandlerFeatures{RecursiveReadOnlyMounts: true}
	metadata := sandboxstore.Metadata{
		RuntimeHandler:           criconfig.RuntimeSharedNetNS,
		ExecutionRuntimeHandler:  "old-runc",
		ExecutionRuntimeFeatures: persistedFeatures,
	}
	service := &criService{runtimeHandlers: map[string]*criruntime.RuntimeHandler{
		"new-runc":                   {Name: "new-runc", Features: &criruntime.RuntimeHandlerFeatures{}},
		criconfig.RuntimeSharedNetNS: {Name: criconfig.RuntimeSharedNetNS, Features: &criruntime.RuntimeHandlerFeatures{}},
	}}
	handler, err := service.runtimeHandlerForSandbox(metadata, criconfig.RuntimeSharedNetNS)
	require.NoError(t, err)
	assert.Same(t, persistedFeatures, handler.Features)
	assert.True(t, handler.Features.GetRecursiveReadOnlyMounts())

	handler, err = service.runtimeHandlerForSandbox(sandboxstore.Metadata{}, "new-runc")
	require.NoError(t, err)
	assert.Same(t, service.runtimeHandlers["new-runc"], handler)
	_, err = service.runtimeHandlerForSandbox(sandboxstore.Metadata{}, "missing")
	require.ErrorContains(t, err, `failed to find runtime handler "missing"`)
}
