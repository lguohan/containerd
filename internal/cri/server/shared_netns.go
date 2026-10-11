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
	goruntime "runtime"
	"strings"

	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	"github.com/containerd/errdefs"
	"k8s.io/apimachinery/pkg/util/validation"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const sharedNetNSGroupLabel = "containerd.io/shared-netns-group"

func sharedNetNSGroup(config *runtime.PodSandboxConfig, handler string) (string, error) {
	if handler != criconfig.RuntimeSharedNetNS {
		return "", nil
	}
	if goruntime.GOOS != "linux" || config.GetLinux() == nil || config.GetWindows() != nil {
		return "", fmt.Errorf("shared-netns requires a Linux sandbox: %w", errdefs.ErrInvalidArgument)
	}
	namespaces := config.GetLinux().GetSecurityContext().GetNamespaceOptions()
	if namespaces.GetNetwork() != runtime.NamespaceMode_POD {
		return "", fmt.Errorf("shared-netns requires private Pod networking: %w", errdefs.ErrInvalidArgument)
	}
	if userns := namespaces.GetUsernsOptions(); userns != nil && userns.GetMode() != runtime.NamespaceMode_NODE {
		return "", fmt.Errorf("shared-netns does not support Pod user namespaces: %w", errdefs.ErrInvalidArgument)
	}
	if len(config.GetPortMappings()) != 0 {
		return "", fmt.Errorf("shared-netns does not configure port mappings: %w", errdefs.ErrInvalidArgument)
	}
	group := config.GetLabels()[sharedNetNSGroupLabel]
	if group == "" || len(group) > 63 {
		return "", fmt.Errorf("shared-netns requires label %s containing a RuntimeClass name of 1 to 63 characters: %w", sharedNetNSGroupLabel, errdefs.ErrInvalidArgument)
	}
	if failures := validation.IsDNS1123Subdomain(group); len(failures) != 0 {
		return "", fmt.Errorf("invalid shared-netns RuntimeClass name %q: %s: %w", group, strings.Join(failures, "; "), errdefs.ErrInvalidArgument)
	}
	return group, nil
}

func (service *criService) releaseSandboxNetwork(sandbox sandboxstore.Sandbox) error {
	if sandbox.SharedNetNS != nil {
		if service.sharedNetNS == nil {
			return fmt.Errorf("shared-netns manager is unavailable")
		}
		return service.sharedNetNS.Release(*sandbox.SharedNetNS)
	}
	if sandbox.NetNS != nil {
		return sandbox.NetNS.Remove()
	}
	return nil
}

func (service *criService) validateSandboxNetwork(sandbox sandboxstore.Sandbox) error {
	if sandbox.SharedNetNS == nil {
		return nil
	}
	if service.sharedNetNS == nil {
		return fmt.Errorf("shared-netns manager is unavailable")
	}
	return service.sharedNetNS.Validate(*sandbox.SharedNetNS)
}
