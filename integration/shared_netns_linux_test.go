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

package integration

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/containerd/containerd/v2/integration/images"
	podsandboxtypes "github.com/containerd/containerd/v2/internal/cri/server/podsandbox/types"
	"github.com/containerd/containerd/v2/internal/cri/util"
)

var sharedNetNSTest = flag.Bool("shared-netns-test", false, "Run experimental shared-netns tests against an explicitly selected disposable daemon")
var sharedNetNSRestartPIDFile = flag.String("shared-netns-restart-pid-file", "", "PID file of a supervised disposable daemon to restart by exact PID")

const sharedNetNSGroupLabel = "containerd.io/shared-netns-group"

type sharedNetNSMember struct {
	id     string
	config *runtime.PodSandboxConfig
	info   *podsandboxtypes.SandboxInfo
}

func requireSharedNetNSDaemon(test *testing.T) {
	test.Helper()
	if !*sharedNetNSTest {
		test.Skip("requires -shared-netns-test and a disposable -cri-endpoint")
	}
	endpoint := strings.TrimPrefix(*criEndpoint, "unix://")
	require.True(test, filepath.IsAbs(endpoint), "shared-netns tests require a local absolute socket path")
	resolved, err := filepath.EvalSymlinks(endpoint)
	require.NoError(test, err)
	require.NotEqual(test, "/run/containerd/containerd.sock", resolved, "refusing the default host daemon")
	require.NotEqual(test, "/run/docker/containerd/containerd.sock", resolved, "refusing the Docker daemon")
	require.Zero(test, os.Geteuid(), "shared-netns integration tests need root for namespace identity checks")
}

func sharedNetNSConfig(name, group string) *runtime.PodSandboxConfig {
	return PodSandboxConfig(name, "shared-netns", WithPodSecurityContext(true), WithPodLabels(map[string]string{
		sharedNetNSGroupLabel: group,
	}))
}

func runSharedNetNSMember(test *testing.T, name, group string) sharedNetNSMember {
	test.Helper()
	config := sharedNetNSConfig(name, group)
	sandboxID, err := runtimeService.RunPodSandbox(config, "shared-netns")
	require.NoError(test, err)
	test.Cleanup(func() {
		assert.NoError(test, runtimeService.StopPodSandbox(sandboxID))
		assert.NoError(test, runtimeService.RemovePodSandbox(sandboxID))
	})
	status, info, err := SandboxInfo(sandboxID)
	require.NoError(test, err)
	require.Equal(test, runtime.PodSandboxState_SANDBOX_READY, status.State)
	require.Equal(test, runtime.NamespaceMode_POD, status.GetLinux().GetNamespaces().GetOptions().GetNetwork())
	require.Empty(test, status.GetNetwork().GetIp())
	require.Empty(test, status.GetNetwork().GetAdditionalIps())
	require.Nil(test, info.CNIResult)
	require.NotNil(test, info.Metadata)
	require.NotNil(test, info.Metadata.SharedNetNS)
	require.Equal(test, group, info.Metadata.SharedNetNS.Group)
	require.Equal(test, "shared-netns", info.Metadata.RuntimeHandler)
	require.NotEmpty(test, info.Metadata.ExecutionRuntimeHandler)
	require.NotEmpty(test, info.Metadata.ExecutionRuntimeConfig)
	require.Equal(test, info.Metadata.SharedNetNS.Path, info.Metadata.NetNSPath)
	assertSharedNetNSIdentity(test, info.Metadata.SharedNetNS.CanonicalPath, info.Metadata.NetNSPath,
		fmt.Sprintf("/proc/%d/ns/net", info.Pid))
	return sharedNetNSMember{id: sandboxID, config: config, info: info}
}

func assertSharedNetNSIdentity(test *testing.T, expected string, paths ...string) {
	test.Helper()
	expectedInfo, err := os.Stat(expected)
	require.NoError(test, err)
	for _, path := range paths {
		actualInfo, err := os.Stat(path)
		require.NoError(test, err)
		require.True(test, os.SameFile(expectedInfo, actualInfo), "%s and %s must refer to the same namespace", expected, path)
	}
}

func sharedNetNSContainer(test *testing.T, member sharedNetNSMember, name, image, command string) string {
	test.Helper()
	config := ContainerConfig(name, image, WithSecurityContext(true), WithCommand("sh", "-c", command))
	containerID, err := runtimeService.CreateContainer(member.id, config, member.config)
	require.NoError(test, err)
	require.NoError(test, runtimeService.StartContainer(containerID))
	container, err := containerdClient.LoadContainer(context.Background(), containerID)
	require.NoError(test, err)
	task, err := container.Task(context.Background(), nil)
	require.NoError(test, err)
	assertSharedNetNSIdentity(test, member.info.Metadata.SharedNetNS.CanonicalPath, fmt.Sprintf("/proc/%d/ns/net", task.Pid()))
	return containerID
}

func sharedNetNSExec(test *testing.T, containerID string, command ...string) string {
	test.Helper()
	stdout, stderr, err := runtimeService.ExecSync(containerID, command, 10*time.Second)
	require.NoError(test, err, "stderr: %s", stderr)
	return strings.TrimSpace(string(stdout))
}

func assertSharedNetNSHTTP(test *testing.T, containerID, response string) {
	test.Helper()
	require.Eventually(test, func() bool {
		stdout, _, err := runtimeService.ExecSync(containerID, []string{"wget", "-q", "-T", "2", "-O", "-", "http://127.0.0.1:18080"}, 3*time.Second)
		return err == nil && strings.TrimSpace(string(stdout)) == response
	}, 15*time.Second, 100*time.Millisecond)
}

func TestSharedNetNSLifecycle(test *testing.T) {
	requireSharedNetNSDaemon(test)
	image := images.Get(images.BusyBox)
	EnsureImageExists(test, image)
	group := "asic0-" + util.GenerateID()[:16]
	first := runSharedNetNSMember(test, "member-a", group)
	second := runSharedNetNSMember(test, "member-b", group)
	other := runSharedNetNSMember(test, "member-c", "asic1-"+util.GenerateID()[:16])
	require.NotEqual(test, first.id, second.id)
	require.NotEqual(test, first.config.Metadata.Uid, second.config.Metadata.Uid)
	require.NotEqual(test, first.config.Metadata.Namespace, second.config.Metadata.Namespace)
	require.NotEqual(test, first.info.Pid, second.info.Pid)
	require.NotEqual(test, first.info.Metadata.NetNSPath, second.info.Metadata.NetNSPath)
	canonical := first.info.Metadata.SharedNetNS.CanonicalPath
	assertSharedNetNSIdentity(test, canonical, second.info.Metadata.NetNSPath)
	canonicalInfo, err := os.Stat(canonical)
	require.NoError(test, err)
	otherInfo, err := os.Stat(other.info.Metadata.NetNSPath)
	require.NoError(test, err)
	require.False(test, os.SameFile(canonicalInfo, otherInfo))
	hostInfo, err := os.Stat("/proc/self/ns/net")
	require.NoError(test, err)
	require.False(test, os.SameFile(canonicalInfo, hostInfo))

	firstContainer := sharedNetNSContainer(test, first, "client", image, "sleep 3600")
	require.Equal(test, "lo", sharedNetNSExec(test, firstContainer, "sh", "-c", "ls /sys/class/net"))
	require.Equal(test, "0x8", sharedNetNSExec(test, firstContainer, "cat", "/sys/class/net/lo/flags"))
	require.Empty(test, sharedNetNSExec(test, firstContainer, "ip", "route", "show"))
	sharedNetNSExec(test, firstContainer, "ip", "link", "set", "lo", "up")
	sharedNetNSExec(test, firstContainer, "ip", "address", "add", "192.0.2.143/32", "dev", "lo")
	secondContainer := sharedNetNSContainer(test, second, "server", image, "mkdir -p /tmp/www && echo shared-netns-ok >/tmp/www/index.html && exec httpd -f -p 127.0.0.1:18080 -h /tmp/www")
	assertSharedNetNSHTTP(test, firstContainer, "shared-netns-ok")
	require.Contains(test, sharedNetNSExec(test, secondContainer, "ip", "address", "show", "dev", "lo"), "192.0.2.143/32")
	status, err := runtimeService.PodSandboxStatus(second.id)
	require.NoError(test, err)
	require.Empty(test, status.GetNetwork().GetIp(), "workload addresses must not become reported Pod IPs")

	require.NoError(test, runtimeService.StopPodSandbox(first.id))
	require.NoError(test, runtimeService.RemovePodSandbox(first.id))
	require.NoError(test, runtimeService.StopPodSandbox(first.id))
	require.NoError(test, runtimeService.RemovePodSandbox(first.id))
	_, err = os.Stat(first.info.Metadata.NetNSPath)
	require.ErrorIs(test, err, os.ErrNotExist)
	assertSharedNetNSHTTP(test, secondContainer, "shared-netns-ok")
	status, survivingInfo, err := SandboxInfo(second.id)
	require.NoError(test, err)
	require.Equal(test, runtime.PodSandboxState_SANDBOX_READY, status.State)
	require.Equal(test, second.info.Pid, survivingInfo.Pid)
	assertSharedNetNSIdentity(test, canonical, second.info.Metadata.NetNSPath)

	require.NoError(test, runtimeService.StopPodSandbox(second.id))
	require.NoError(test, runtimeService.RemovePodSandbox(second.id))
	_, err = os.Stat(second.info.Metadata.NetNSPath)
	require.ErrorIs(test, err, os.ErrNotExist)
	retainedInfo, err := os.Stat(canonical)
	require.NoError(test, err)
	require.True(test, os.SameFile(canonicalInfo, retainedInfo), "last-member removal must retain the namespace")
	rejoined := runSharedNetNSMember(test, "member-d", group)
	assertSharedNetNSIdentity(test, canonical, rejoined.info.Metadata.NetNSPath)
	rejoinedContainer := sharedNetNSContainer(test, rejoined, "rejoined", image, "sleep 3600")
	require.Contains(test, sharedNetNSExec(test, rejoinedContainer, "ip", "address", "show", "dev", "lo"), "192.0.2.143/32")
	require.NotEqual(test, "0x8", sharedNetNSExec(test, rejoinedContainer, "cat", "/sys/class/net/lo/flags"))
}

func TestSharedNetNSWithoutCNI(test *testing.T) {
	requireSharedNetNSDaemon(test)
	response, err := runtimeService.Status()
	require.NoError(test, err)
	networkUnready := false
	for _, condition := range response.GetStatus().GetConditions() {
		if condition.Type == runtime.NetworkReady {
			networkUnready = !condition.Status
		}
	}
	if !networkUnready {
		test.Skip("requires a disposable daemon with missing or empty CNI configuration")
	}
	runSharedNetNSMember(test, "without-cni", "no-cni-"+util.GenerateID()[:16])
	config := PodSandboxConfig("ordinary-host-network", "shared-netns", WithHostNetwork)
	sandboxID, err := runtimeService.RunPodSandbox(config, "")
	require.NoError(test, err, "the ordinary default runtime must remain usable")
	test.Cleanup(func() {
		assert.NoError(test, runtimeService.StopPodSandbox(sandboxID))
		assert.NoError(test, runtimeService.RemovePodSandbox(sandboxID))
	})
}

func TestSharedNetNSValidation(test *testing.T) {
	requireSharedNetNSDaemon(test)
	cases := []struct {
		name   string
		mutate func(*runtime.PodSandboxConfig)
	}{
		{name: "missing-group", mutate: func(config *runtime.PodSandboxConfig) { delete(config.Labels, sharedNetNSGroupLabel) }},
		{name: "empty-group", mutate: func(config *runtime.PodSandboxConfig) { config.Labels[sharedNetNSGroupLabel] = "" }},
		{name: "long-group", mutate: func(config *runtime.PodSandboxConfig) { config.Labels[sharedNetNSGroupLabel] = strings.Repeat("a", 64) }},
		{name: "path-group", mutate: func(config *runtime.PodSandboxConfig) { config.Labels[sharedNetNSGroupLabel] = "../asic0" }},
		{name: "uppercase-group", mutate: func(config *runtime.PodSandboxConfig) { config.Labels[sharedNetNSGroupLabel] = "ASIC0" }},
		{name: "host-network", mutate: WithHostNetwork},
		{name: "user-namespace", mutate: WithPodUserNs(0, 65536, 65536)},
	}
	for _, testcase := range cases {
		test.Run(testcase.name, func(test *testing.T) {
			before, err := runtimeService.ListPodSandbox(nil)
			require.NoError(test, err)
			config := sharedNetNSConfig(testcase.name, "invalid-"+util.GenerateID()[:16])
			testcase.mutate(config)
			sandboxID, err := runtimeService.RunPodSandbox(config, "shared-netns")
			if sandboxID != "" {
				test.Cleanup(func() {
					assert.NoError(test, runtimeService.StopPodSandbox(sandboxID))
					assert.NoError(test, runtimeService.RemovePodSandbox(sandboxID))
				})
			}
			require.Error(test, err)
			require.Empty(test, sandboxID)
			after, err := runtimeService.ListPodSandbox(nil)
			require.NoError(test, err)
			require.Len(test, after, len(before), "rejected requests must not leave a sandbox")
		})
	}
}

func TestSharedNetNSConcurrentMembers(test *testing.T) {
	requireSharedNetNSDaemon(test)
	group := "concurrent-" + util.GenerateID()[:16]
	const memberCount = 6
	configs := make([]*runtime.PodSandboxConfig, memberCount)
	sandboxIDs := make([]string, memberCount)
	createErrors := make([]error, memberCount)
	for index := range configs {
		configs[index] = sharedNetNSConfig(fmt.Sprintf("member-%d", index), group)
	}
	test.Cleanup(func() {
		for _, sandboxID := range sandboxIDs {
			if sandboxID != "" {
				assert.NoError(test, runtimeService.StopPodSandbox(sandboxID))
				assert.NoError(test, runtimeService.RemovePodSandbox(sandboxID))
			}
		}
	})
	var workers sync.WaitGroup
	for index := range configs {
		workers.Go(func() {
			sandboxIDs[index], createErrors[index] = runtimeService.RunPodSandbox(configs[index], "shared-netns")
		})
	}
	workers.Wait()
	canonical := ""
	aliases := make([]string, memberCount)
	for index, sandboxID := range sandboxIDs {
		require.NoError(test, createErrors[index])
		_, info, err := SandboxInfo(sandboxID)
		require.NoError(test, err)
		require.NotNil(test, info.Metadata.SharedNetNS)
		if canonical == "" {
			canonical = info.Metadata.SharedNetNS.CanonicalPath
		}
		aliases[index] = info.Metadata.NetNSPath
		assertSharedNetNSIdentity(test, canonical, aliases[index])
	}
	removeErrors := make([]error, memberCount)
	for index, sandboxID := range sandboxIDs {
		workers.Go(func() {
			if err := runtimeService.StopPodSandbox(sandboxID); err != nil {
				removeErrors[index] = err
				return
			}
			removeErrors[index] = runtimeService.RemovePodSandbox(sandboxID)
		})
	}
	workers.Wait()
	for index := range removeErrors {
		require.NoError(test, removeErrors[index])
		_, err := os.Stat(aliases[index])
		require.ErrorIs(test, err, os.ErrNotExist)
	}
	rejoined := runSharedNetNSMember(test, "after-concurrency", group)
	assertSharedNetNSIdentity(test, canonical, rejoined.info.Metadata.NetNSPath)
}

func TestSharedNetNSDaemonRestart(test *testing.T) {
	requireSharedNetNSDaemon(test)
	if *sharedNetNSRestartPIDFile == "" {
		test.Skip("requires -shared-netns-restart-pid-file for a supervised disposable daemon")
	}
	image := images.Get(images.BusyBox)
	EnsureImageExists(test, image)
	group := "restart-" + util.GenerateID()[:16]
	first := runSharedNetNSMember(test, "restart-a", group)
	second := runSharedNetNSMember(test, "restart-b", group)
	stopped := runSharedNetNSMember(test, "restart-stopped", group)
	require.NoError(test, runtimeService.StopPodSandbox(stopped.id))
	_, err := os.Stat(stopped.info.Metadata.NetNSPath)
	require.ErrorIs(test, err, os.ErrNotExist)
	client := sharedNetNSContainer(test, first, "client", image, "sleep 3600")
	sharedNetNSExec(test, client, "ip", "link", "set", "lo", "up")
	sharedNetNSContainer(test, second, "server", image, "mkdir -p /tmp/www && echo restart-ok >/tmp/www/index.html && exec httpd -f -p 127.0.0.1:18080 -h /tmp/www")
	assertSharedNetNSHTTP(test, client, "restart-ok")
	before, err := os.ReadFile(*sharedNetNSRestartPIDFile)
	require.NoError(test, err)
	daemonPID, err := strconv.Atoi(strings.TrimSpace(string(before)))
	require.NoError(test, err)
	require.Greater(test, daemonPID, 1)
	commandLine, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", daemonPID))
	require.NoError(test, err)
	arguments := strings.Split(string(commandLine), "\x00")
	endpoint := strings.TrimPrefix(*criEndpoint, "unix://")
	matchedAddress := false
	for index, argument := range arguments {
		if argument == "--address="+endpoint || ((argument == "--address" || argument == "-a") && index+1 < len(arguments) && arguments[index+1] == endpoint) {
			matchedAddress = true
		}
	}
	require.True(test, matchedAddress, "refusing to signal a daemon without the explicit test socket in its command line")
	process, err := os.FindProcess(daemonPID)
	require.NoError(test, err)
	clientService, connection, err := RawRuntimeClient()
	require.NoError(test, err)
	defer connection.Close()
	require.NoError(test, process.Signal(syscall.SIGTERM))
	require.Eventually(test, func() bool {
		current, err := os.ReadFile(*sharedNetNSRestartPIDFile)
		if err != nil || strings.TrimSpace(string(current)) == strings.TrimSpace(string(before)) {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err = clientService.Version(ctx, &runtime.VersionRequest{Version: "0.1.0"})
		return err == nil
	}, 45*time.Second, 250*time.Millisecond)
	for _, member := range []sharedNetNSMember{first, second} {
		status, recovered, err := SandboxInfo(member.id)
		require.NoError(test, err)
		require.Equal(test, runtime.PodSandboxState_SANDBOX_READY, status.State)
		require.Empty(test, status.GetNetwork().GetIp())
		require.Equal(test, member.info.Pid, recovered.Pid)
		require.Equal(test, member.info.Metadata.SharedNetNS, recovered.Metadata.SharedNetNS)
		assertSharedNetNSIdentity(test, member.info.Metadata.SharedNetNS.CanonicalPath, recovered.Metadata.NetNSPath,
			fmt.Sprintf("/proc/%d/ns/net", recovered.Pid))
	}
	assertSharedNetNSHTTP(test, client, "restart-ok")
	stoppedStatus, err := runtimeService.PodSandboxStatus(stopped.id)
	require.NoError(test, err)
	require.Equal(test, runtime.PodSandboxState_SANDBOX_NOTREADY, stoppedStatus.State)
	require.NoError(test, runtimeService.StopPodSandbox(stopped.id))
	require.NoError(test, runtimeService.RemovePodSandbox(stopped.id))
	assertSharedNetNSHTTP(test, client, "restart-ok")
	rejoined := runSharedNetNSMember(test, "restart-c", group)
	assertSharedNetNSIdentity(test, first.info.Metadata.SharedNetNS.CanonicalPath, rejoined.info.Metadata.NetNSPath)
}
