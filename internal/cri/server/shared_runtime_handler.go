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
	"fmt"

	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func (service *criService) registerSharedNetNSRuntimeHandler() {
	runtimeName, _, err := service.config.ResolveSandboxRuntime(nil, criconfig.RuntimeSharedNetNS)
	if err != nil {
		return
	}
	defaultHandler, exists := service.runtimeHandlers[runtimeName]
	if !exists {
		return
	}
	sharedHandler := &runtime.RuntimeHandler{Name: criconfig.RuntimeSharedNetNS}
	if defaultHandler.Features != nil {
		sharedHandler.Features = &runtime.RuntimeHandlerFeatures{
			RecursiveReadOnlyMounts: defaultHandler.Features.RecursiveReadOnlyMounts,
		}
	}
	service.runtimeHandlers[criconfig.RuntimeSharedNetNS] = sharedHandler
}

func (service *criService) runtimeHandlerForSandbox(metadata sandboxstore.Metadata, runtimeHandlerName string) (*runtime.RuntimeHandler, error) {
	if metadata.RuntimeHandler == criconfig.RuntimeSharedNetNS {
		return &runtime.RuntimeHandler{
			Name:     criconfig.RuntimeSharedNetNS,
			Features: metadata.ExecutionRuntimeFeatures,
		}, nil
	}
	runtimeHandler, exists := service.runtimeHandlers[runtimeHandlerName]
	if !exists {
		return nil, fmt.Errorf("failed to find runtime handler %q", runtimeHandlerName)
	}
	return runtimeHandler, nil
}
