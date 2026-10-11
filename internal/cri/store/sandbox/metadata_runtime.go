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
	"fmt"

	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	"github.com/pelletier/go-toml/v2"
)

func (metadata *Metadata) GetRuntime(config *criconfig.Config) (criconfig.Runtime, error) {
	if metadata.RuntimeHandler != criconfig.RuntimeSharedNetNS {
		return config.GetSandboxRuntime(metadata.Config, metadata.RuntimeHandler)
	}
	if metadata.ExecutionRuntimeHandler == "" || metadata.ExecutionRuntimeHandler == criconfig.RuntimeSharedNetNS || metadata.ExecutionRuntimeConfig == "" {
		return criconfig.Runtime{}, fmt.Errorf("shared-netns sandbox %q has no saved execution runtime", metadata.ID)
	}
	var executionRuntime criconfig.Runtime
	if err := toml.Unmarshal([]byte(metadata.ExecutionRuntimeConfig), &executionRuntime); err != nil {
		return criconfig.Runtime{}, fmt.Errorf("decode saved execution runtime for sandbox %q: %w", metadata.ID, err)
	}
	if executionRuntime.Type != "io.containerd.runc.v2" || (executionRuntime.Sandboxer != "" && executionRuntime.Sandboxer != string(criconfig.ModePodSandbox)) {
		return criconfig.Runtime{}, fmt.Errorf("shared-netns sandbox %q has an incompatible saved execution runtime", metadata.ID)
	}
	return executionRuntime, nil
}
