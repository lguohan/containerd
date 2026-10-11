# Experimental shared network namespaces

This prototype explores [containerd issue 14315](https://github.com/containerd/containerd/issues/14315).
Separate CRI Pod sandboxes on one Linux node can join a private network
namespace selected by a group name while retaining separate sandbox and
container lifecycles. It is intended for direct CRI experiments with trusted
workloads such as services for one SONiC ASIC. It is not an accepted upstream
Kubernetes networking interface or a supported production feature.

## Interface

Call `RunPodSandbox` with the literal runtime handler `shared-netns` and the
label `containerd.io/shared-netns-group` in `PodSandboxConfig.labels`:

```json
{
  "runtime_handler": "shared-netns",
  "config": {
    "metadata": {
      "name": "member-a",
      "namespace": "sonic",
      "uid": "member-a-unique-uid",
      "attempt": 0
    },
    "labels": {
      "containerd.io/shared-netns-group": "asic0"
    },
    "linux": {
      "security_context": {
        "namespace_options": {
          "network": "POD"
        }
      }
    }
  }
}
```

The handler delegates execution to the existing `default_runtime_name` and
uses its runtime options and snapshotter. No runtime entry named `shared-netns`,
per-group configuration, supplied namespace path, or IP address is required.
The prototype supports the Linux runc v2 runtime with the pod sandbox
controller; incompatible defaults are rejected. A configured runtime named
`shared-netns` conflicts with the reserved experimental handler name.

Group names must be valid RuntimeClass-style DNS subdomain names of at most
63 characters. Empty, invalid, and longer names are rejected. Names are not
truncated or hashed into alternative group identities. A filesystem-safe
internal name does not change the stored group identity.

The scope is the daemon's node: `asic0` and `asic1` are separate groups, but
Pods in different Kubernetes API namespaces using `asic0` share the same
network stack on this node. Callers must arrange placement and authorization.
The label is a transport value, not proof of admission or permission to join.
Containerd does not read RuntimeClass objects or enforce Kubernetes RBAC.

Requests must use a private Linux network namespace. Host networking and
per-Pod user namespaces are rejected. The prototype does not provide separate
network identities, port allocation, host-port forwarding, or per-member
NetworkPolicy enforcement.

## Namespace ownership and lifetime

The first sandbox creates a canonical namespace owned by containerd. Each
member gets its own namespace mount alias. Workload containers join their own
sandbox in the usual way, so all members of a group reach the same namespace.

Containerd does not run CNI or configure links, addresses, or routes for this
handler. The initial loopback device remains in its kernel-created state,
including being down. A privileged workload can configure it, for example
with `ip link set lo up`, before using localhost. The CRI sandbox reports no
Pod IPs, including after a workload adds addresses to its interfaces.

Stopping/removing one member releases only its alias and membership after
its resources are stopped. The canonical mount remains while other members
exist. **The empty group is also retained after the final member is removed.**
A later sandbox using the same group rejoins the retained namespace and sees
the existing network configuration. Names therefore represent persistent
node-local groups rather than a generation of a Kubernetes RuntimeClass.
Deleting and recreating a RuntimeClass with the same name does not reset them.

Durable sandbox metadata records the group membership and the execution
runtime selected at sandbox creation. Cleanup and recovery use this stored
selection instead of new labels or the current default runtime. Namespace
identity includes the boot identifier and device/inode identity, so a different
namespace appearing at the same path must not be accepted as the old one.
Retained mounts can survive a containerd process restart on the same boot;
they cannot preserve a kernel namespace across a node reboot.

Frozen runtime settings still depend on the selected shim/runc binaries,
snapshotter plugin, and referenced execution resources remaining available.
OCI base-spec file contents are not checkpointed. A `base_runtime_spec` path
must remain cached through at least one configured runtime after restart;
removing its last runtime configuration can prevent new containers in a
recovered sandbox from starting. The sandbox does not silently switch to the
new default runtime.

Only one namespace manager may own a root directory at a time. Operations
are serialized within the CRI service; the manager does not provide a lock
between daemon processes sharing that directory. Each test daemon must use
its own root and state directories.

A crash after creating the canonical mount but before persisting its
device/inode identity leaves ownership uncertain. A subsequent acquisition
fails closed rather than adopting or removing that mount. An operator must
inspect and resolve the state before the affected group can be used again; fully
automatic recovery across this persistence window is outside this prototype.

Changed mounts, corrupt journals, or unresolved live memberships can block
recovery of the entire CRI service, including ordinary sandboxes. A persistent
state directory can also contain unmounted regular files after reboot, which
are rejected instead of being mistaken for namespaces. A fresh boot with an
ephemeral `/run` state directory avoids those stale mount placeholders, but
does not retain network configuration. These fail-closed limits are why this
prototype is not production-ready.

There is no group-delete RPC or TTL in this prototype. Retained groups consume
kernel resources. Use a disposable daemon and tear down its private mount
namespace after testing. Do not recursively delete mounted namespace paths or
delete aliases belonging to active sandboxes.

Every member can observe the same interfaces, routes, ports and localhost.
A member with network administration privileges can affect all its peers.
Only mutually trusted workloads should join a group. Kubernetes namespace
boundaries do not provide network isolation inside that group.

## Kubernetes compatibility

The proposed RuntimeClass/admission arrangement can transport a group label,
but this prototype does not install admission resources or establish support
for IP-less Pods in released Kubernetes. In Kubernetes v1.37.1,
[`PodSandboxChanged`](https://github.com/kubernetes/kubernetes/blob/v1.37.1/pkg/kubelet/kuberuntime/util/util.go)
replaces a ready non-host-network sandbox when its non-nil network status has
an empty primary IP. Omitting the network status merely bypasses this
individual check; it is not a supported compatibility mechanism.

[KEP-6313](https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/6313-network-isolated-pods)
defines `spec.defaultNetwork: None`, a typed CRI request, capability discovery,
and intentional IP-less Pod semantics. As checked on October 11, 2026, the KEP
is merged and targets alpha in Kubernetes v1.38, while
[the Kubernetes implementation](https://github.com/kubernetes/kubernetes/pull/141604)
remains a draft. Its contract brings loopback up and keeps Pod IPs empty even
when external integrations later add interfaces. This prototype leaves
loopback untouched and does not implement the new typed CRI field.

The KEP describes a Pod's own isolated namespace; cross-Pod sharing and joining
an already-configured group require a separate agreed contract. Containerd
maintainers requested Kubernetes-level design discussion in
[issue 14315](https://github.com/containerd/containerd/issues/14315#issuecomment-6089915814).
Direct CRI tests here demonstrate namespace mechanics, not Kubernetes
conformance, working Services, network probes, or NetworkPolicy integration.

## Running the focused tests

The reproducible harness builds the binaries as the invoking user, then uses
`sudo` to run only its test daemon in private mount and PID namespaces:

```sh
script/test-shared-netns --run
```

It requires Linux, Go, make, runc, and util-linux `unshare`. Use `--no-build`
to reuse binaries in `bin/`. `GO` can select the Go executable,
`GOMAXPROCS` controls build concurrency (default 2), and `TEST_IMAGE_LIST` can
select the existing integration image override file. Its native snapshotter
uses local image pulling, and its CNI configuration is deliberately empty.
The harness supervises only its own daemon to exercise restart recovery,
including a stopped sandbox awaiting removal. No system service is restarted.
Exiting the private PID namespace removes all its remaining processes and
namespace mounts. The unique `/tmp/containerd-shared-netns.*` directory is
retained for inspection, with `tests.log` and `daemon.log`; it also contains
the disposable image store and may be removed after the harness exits.

To run the binary against a manually managed disposable daemon instead:

Use a separate daemon with its own root, state, Unix socket, CRI streaming
port, and empty CNI configuration directory. Do not replace the host daemon,
restart system services, or run `make clean-test` on a shared host. The suite
rejects the usual host/Docker sockets and requires explicit opt-in; choosing
a genuinely disposable endpoint remains the operator's responsibility.

Build the integration test binary, then run it as root against that endpoint:

```sh
go test -c -o /tmp/shared-netns-integration.test ./integration
sudo /tmp/shared-netns-integration.test \
  -test.run '^TestSharedNetNS' -test.v \
  -cri-endpoint unix:///tmp/shared-netns-test/containerd.sock \
  -shared-netns-test
```

The daemon needs a usable runc v2 shim, a sandbox image, and the BusyBox test
image with `ip`, `httpd`, and `wget`. Existing integration `-image-list`
overrides can select preloaded images. Pulling images requires the daemon's
network; workload sandboxes do not need CNI.

The tests exercise same-group identity, different-group and host isolation,
workload placement, untouched loopback, HTTP over shared localhost, unchanged
survivors, idempotent removal, retained network configuration, concurrent
creation/removal, input validation, and operation with unready CNI. The
no-CNI test skips when the configured daemon already reports `NetworkReady`.
Inspect the test output for skips before claiming coverage.

The restart case is optional and skips unless passed
`-shared-netns-restart-pid-file /path/to/daemon.pid`. A supervisor must write
that dedicated daemon's PID, restart it after SIGTERM, and replace the PID file.
The daemon must be launched with `--address /absolute/test/socket` (or `-a`)
on its command line. The test verifies that exact socket before signaling only
that PID, then checks sandbox PIDs, namespace identities, continuing traffic,
and new-member joins. It never uses `pkill` or the global restart helpers.

These tests do not simulate power loss, node reboot, arbitrary mount tampering,
or Kubernetes lifecycle behavior. Namespace manager tests cover additional
concurrency and failure/recovery cases without a Kubernetes API server.
