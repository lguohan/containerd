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

// Package sharednetns manages retained, unconfigured network namespaces for CRI.
package sharednetns

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/containerd/containerd/v2/pkg/atomicfile"
	"github.com/containerd/containerd/v2/pkg/identifiers"
	"github.com/containerd/errdefs"
)

// Membership records the exact namespace acquired by a sandbox.
type Membership struct {
	Group         string `json:"group"`
	SandboxID     string `json:"sandboxID"`
	Path          string `json:"path"`
	CanonicalPath string `json:"canonicalPath"`
	Device        uint64 `json:"device"`
	Inode         uint64 `json:"inode"`
	Generation    string `json:"generation"`
	BootID        string `json:"bootID"`
}

type identity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type namespaceBackend interface {
	Create(string) (identity, error)
	Bind(string, string, identity) error
	Inspect(string) (identity, error)
	Remove(string, identity) error
}

type groupState struct {
	Group         string                `json:"group"`
	CanonicalPath string                `json:"canonicalPath"`
	Generation    string                `json:"generation"`
	BootID        string                `json:"bootID"`
	Identity      identity              `json:"identity"`
	Members       map[string]Membership `json:"members"`
}

type manifest struct {
	Version int                    `json:"version"`
	Groups  map[string]*groupState `json:"groups"`
}

// Manager serializes namespace operations for one CRI service instance.
// Only one Manager may own a root directory at a time.
type Manager struct {
	mu       sync.Mutex
	rootDir  string
	stateDir string
	bootID   string
	backend  namespaceBackend
	groups   map[string]*groupState
	write    func(string, []byte) error
}

var groupPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func newManager(rootDir, stateDir, bootID string, backend namespaceBackend) (*Manager, error) {
	if !filepath.IsAbs(rootDir) || !filepath.IsAbs(stateDir) || bootID == "" {
		return nil, fmt.Errorf("shared namespace directories must be absolute and boot identity must be present: %w", errdefs.ErrInvalidArgument)
	}
	manager := &Manager{
		rootDir: filepath.Clean(rootDir), stateDir: filepath.Clean(stateDir),
		bootID: bootID, backend: backend, groups: map[string]*groupState{}, write: writeManifest,
	}
	for _, directory := range []string{manager.rootDir, filepath.Join(manager.stateDir, "groups"), filepath.Join(manager.stateDir, "sandboxes")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(filepath.Join(manager.rootDir, "groups.json"))
	if os.IsNotExist(err) {
		return manager, nil
	}
	if err != nil {
		return nil, err
	}
	var saved manifest
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("decode shared network namespaces: %w", err)
	}
	if saved.Version != 1 || saved.Groups == nil {
		return nil, fmt.Errorf("invalid shared network namespace manifest")
	}
	for name, group := range saved.Groups {
		if group == nil || group.Group != name || validateGroup(name) != nil || group.BootID == "" || !validGeneration(group.Generation) || group.Members == nil || group.CanonicalPath != manager.canonicalPath(group.Generation) {
			return nil, fmt.Errorf("invalid shared network namespace group %q", name)
		}
		for sandboxID, member := range group.Members {
			if identifiers.Validate(sandboxID) != nil || member != manager.membership(group, sandboxID) {
				return nil, fmt.Errorf("invalid shared network namespace membership %q", sandboxID)
			}
		}
	}
	manager.groups = saved.Groups
	return manager, nil
}

// Acquire pins a group's namespace through a private alias for the sandbox.
// Empty groups retain their namespace and its configuration between acquisitions.
func (manager *Manager) Acquire(groupName, sandboxID string) (Membership, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if err := validateGroup(groupName); err != nil {
		return Membership{}, err
	}
	if err := identifiers.Validate(sandboxID); err != nil {
		return Membership{}, err
	}
	for name, group := range manager.groups {
		if _, exists := group.Members[sandboxID]; exists && name != groupName {
			return Membership{}, fmt.Errorf("sandbox %q already belongs to group %q: %w", sandboxID, name, errdefs.ErrAlreadyExists)
		}
	}
	group := manager.groups[groupName]
	if group != nil && group.BootID != manager.bootID {
		if len(group.Members) != 0 {
			return Membership{}, fmt.Errorf("group %q has memberships from a previous boot: %w", groupName, errdefs.ErrFailedPrecondition)
		}
		group = nil
	}
	if group == nil {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return Membership{}, err
		}
		generation := hex.EncodeToString(random)
		group = &groupState{Group: groupName, CanonicalPath: manager.canonicalPath(generation), Generation: generation, BootID: manager.bootID, Members: map[string]Membership{}}
		previous := manager.groups[groupName]
		manager.groups[groupName] = group
		if err := manager.persist(); err != nil {
			if previous == nil {
				delete(manager.groups, groupName)
			} else {
				manager.groups[groupName] = previous
			}
			return Membership{}, err
		}
	}
	if group.Identity == (identity{}) {
		if _, err := manager.backend.Inspect(group.CanonicalPath); !os.IsNotExist(err) {
			return Membership{}, fmt.Errorf("group %q has an incomplete namespace creation; refusing to adopt an unverified path: %w", groupName, errdefs.ErrFailedPrecondition)
		}
		created, err := manager.backend.Create(group.CanonicalPath)
		if err != nil {
			return Membership{}, fmt.Errorf("create shared namespace: %w", err)
		}
		group.Identity = created
		if err := manager.persist(); err != nil {
			if cleanupErr := manager.backend.Remove(group.CanonicalPath, created); cleanupErr != nil {
				return Membership{}, errors.Join(err, cleanupErr)
			}
			group.Identity = identity{}
			return Membership{}, errors.Join(err, manager.persist())
		}
	}
	if err := manager.check(group.CanonicalPath, group.Identity); err != nil {
		return Membership{}, err
	}
	member := manager.membership(group, sandboxID)
	_, alreadyMember := group.Members[sandboxID]
	rollback := func(err error) (Membership, error) {
		if !alreadyMember {
			err = errors.Join(err, manager.release(member))
		}
		return Membership{}, err
	}
	group.Members[sandboxID] = member
	if err := manager.persist(); err != nil {
		return rollback(err)
	}
	actual, err := manager.backend.Inspect(member.Path)
	if os.IsNotExist(err) {
		if err := manager.backend.Bind(group.CanonicalPath, member.Path, group.Identity); err != nil {
			return rollback(fmt.Errorf("pin shared namespace for sandbox %q: %w", sandboxID, err))
		}
	} else if err != nil {
		return rollback(err)
	} else if actual != group.Identity {
		return rollback(fmt.Errorf("sandbox namespace %q was replaced: %w", member.Path, errdefs.ErrFailedPrecondition))
	}
	if err := manager.check(member.Path, group.Identity); err != nil {
		return rollback(err)
	}
	return member, nil
}

// Validate verifies the persisted membership, boot, and both namespace mounts.
func (manager *Manager) Validate(member Membership) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.validate(member)
}

func (manager *Manager) validate(member Membership) error {
	group := manager.groups[member.Group]
	if group == nil || group.Members[member.SandboxID] != member {
		return fmt.Errorf("unknown shared namespace membership for %q: %w", member.SandboxID, errdefs.ErrFailedPrecondition)
	}
	if member.BootID != manager.bootID {
		return fmt.Errorf("shared namespace for %q belongs to a previous boot: %w", member.SandboxID, errdefs.ErrFailedPrecondition)
	}
	if err := manager.check(member.CanonicalPath, group.Identity); err != nil {
		return err
	}
	return manager.check(member.Path, group.Identity)
}

// Release removes only this sandbox's alias. Repeated releases are harmless.
func (manager *Manager) Release(member Membership) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.release(member)
}

func (manager *Manager) release(member Membership) error {
	group := manager.groups[member.Group]
	if group == nil {
		return nil
	}
	stored, exists := group.Members[member.SandboxID]
	if !exists {
		return nil
	}
	if stored != member {
		return fmt.Errorf("shared namespace membership for %q changed: %w", member.SandboxID, errdefs.ErrFailedPrecondition)
	}
	if member.BootID == manager.bootID {
		if err := manager.backend.Remove(member.Path, group.Identity); err != nil {
			return fmt.Errorf("release sandbox shared namespace: %w", err)
		}
	}
	delete(group.Members, member.SandboxID)
	if err := manager.persist(); err != nil {
		group.Members[member.SandboxID] = member
		return err
	}
	return nil
}

// Recover reconciles aliases with authoritative sandbox metadata during startup.
// Invalid live memberships are retained and returned as errors for the caller.
func (manager *Manager) Recover(members []Membership) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	authoritative := make(map[string]Membership, len(members))
	var failures []error
	for _, member := range members {
		if previous, exists := authoritative[member.SandboxID]; exists && previous != member {
			return fmt.Errorf("conflicting recovered memberships for sandbox %q", member.SandboxID)
		}
		authoritative[member.SandboxID] = member
		group := manager.groups[member.Group]
		var stored Membership
		if group != nil {
			stored = group.Members[member.SandboxID]
		}
		if manager.validMembershipPaths(member) && (stored == (Membership{}) || stored == member) {
			if _, err := manager.backend.Inspect(member.Path); os.IsNotExist(err) {
				if err := manager.release(member); err != nil {
					failures = append(failures, err)
				}
				continue
			}
		}
		if err := manager.validate(member); err != nil {
			failures = append(failures, err)
		}
	}
	for _, group := range manager.groups {
		for sandboxID, member := range group.Members {
			if _, exists := authoritative[sandboxID]; !exists {
				if err := manager.release(member); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	return errors.Join(failures...)
}

// DeleteGroup explicitly removes an empty group, for administrative teardown.
// Normal sandbox cleanup must use Release so that network configuration survives.
func (manager *Manager) DeleteGroup(groupName string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	group := manager.groups[groupName]
	if group == nil {
		return nil
	}
	if len(group.Members) != 0 {
		return fmt.Errorf("shared namespace group %q still has members: %w", groupName, errdefs.ErrFailedPrecondition)
	}
	if group.BootID == manager.bootID {
		if err := manager.backend.Remove(group.CanonicalPath, group.Identity); err != nil {
			return err
		}
	}
	delete(manager.groups, groupName)
	if err := manager.persist(); err != nil {
		manager.groups[groupName] = group
		return err
	}
	return nil
}

func (manager *Manager) canonicalPath(generation string) string {
	return filepath.Join(manager.stateDir, "groups", generation)
}

func (manager *Manager) validMembershipPaths(member Membership) bool {
	return validateGroup(member.Group) == nil && identifiers.Validate(member.SandboxID) == nil &&
		validGeneration(member.Generation) && member.BootID != "" &&
		member.CanonicalPath == manager.canonicalPath(member.Generation) &&
		member.Path == filepath.Join(manager.stateDir, "sandboxes", member.SandboxID)
}

func (manager *Manager) membership(group *groupState, sandboxID string) Membership {
	return Membership{
		Group: group.Group, SandboxID: sandboxID, Path: filepath.Join(manager.stateDir, "sandboxes", sandboxID),
		CanonicalPath: group.CanonicalPath, Device: group.Identity.Device, Inode: group.Identity.Inode,
		Generation: group.Generation, BootID: group.BootID,
	}
}

func (manager *Manager) check(path string, expected identity) error {
	actual, err := manager.backend.Inspect(path)
	if err != nil {
		return fmt.Errorf("inspect shared namespace %q: %w", path, err)
	}
	if actual != expected {
		return fmt.Errorf("shared namespace %q was replaced: %w", path, errdefs.ErrFailedPrecondition)
	}
	return nil
}

func (manager *Manager) persist() error {
	data, err := json.Marshal(manifest{Version: 1, Groups: manager.groups})
	if err != nil {
		return err
	}
	return manager.write(filepath.Join(manager.rootDir, "groups.json"), data)
}

func writeManifest(path string, data []byte) error {
	file, err := atomicfile.New(path, 0600)
	if err != nil {
		return err
	}
	defer file.Cancel()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateGroup(group string) error {
	if len(group) == 0 || len(group) > 63 || !groupPattern.MatchString(group) {
		return fmt.Errorf("shared namespace group must be a RuntimeClass name of at most 63 characters: %w", errdefs.ErrInvalidArgument)
	}
	return nil
}

func validGeneration(generation string) bool {
	decoded, err := hex.DecodeString(generation)
	return err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == generation
}
