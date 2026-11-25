package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus/v7/shared/api"
)

// snapshotVersion is the current version of the snapshot file format.
const snapshotVersion = 1

// redactedValue replaces the value of sensitive configuration keys.
const redactedValue = "<redacted>"

// serverConfigKeys lists the server configuration keys relevant to instance placement.
var serverConfigKeys = []string{
	"cluster.healing_threshold",
	"cluster.offline_threshold",
	"cluster.rebalance.batch",
	"cluster.rebalance.cooldown",
	"cluster.rebalance.interval",
	"cluster.rebalance.threshold",
	"images.default_architecture",
	"instances.placement.scriptlet",
}

// Snapshot is a description of a cluster with everything the instance placement scriptlet can see.
type Snapshot struct {
	Version    int                `yaml:"version"`
	CapturedAt time.Time          `yaml:"captured_at"`
	Server     SnapshotServer     `yaml:"server"`
	Members    []SnapshotMember   `yaml:"members"`
	Groups     []api.ClusterGroup `yaml:"groups"`
	Projects   []api.Project      `yaml:"projects"`
	Profiles   []api.Profile      `yaml:"profiles"`
	Instances  []api.Instance     `yaml:"instances"`
}

// SnapshotServer holds the server wide details of a snapshot.
type SnapshotServer struct {
	Version string            `yaml:"version"`
	Config  map[string]string `yaml:"config"`
}

// SnapshotMember holds a cluster member along with its resources and state.
type SnapshotMember struct {
	api.ClusterMember `yaml:",inline"`

	Resources *api.Resources          `yaml:"resources,omitempty"`
	State     *api.ClusterMemberState `yaml:"state,omitempty"`
}

// loadSnapshot reads a snapshot file.
func loadSnapshot(path string) (*Snapshot, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	snap := &Snapshot{}

	err = yaml.Unmarshal(content, snap)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing snapshot %q: %w", path, err)
	}

	if snap.Version != snapshotVersion {
		return nil, fmt.Errorf("Unsupported snapshot version %d in %q", snap.Version, path)
	}

	if len(snap.Members) == 0 {
		return nil, errors.New("Snapshot doesn't contain any cluster member")
	}

	return snap, nil
}

// write stores the snapshot at the given path, using standard output for "-".
func (s *Snapshot) write(path string) error {
	content, err := yaml.Marshal(s)
	if err != nil {
		return err
	}

	if path == "-" {
		_, err = os.Stdout.Write(content)
		return err
	}

	return os.WriteFile(path, content, 0o600)
}

// isSensitiveKey returns whether a configuration key is likely to hold secrets.
func isSensitiveKey(key string) bool {
	if strings.HasPrefix(key, "cloud-init.") || strings.HasPrefix(key, "environment.") {
		return true
	}

	switch key {
	case "user.user-data", "user.vendor-data", "user.network-config", "user.meta-data":
		return true
	}

	return false
}

// redactConfig replaces the values of sensitive keys in place.
func redactConfig(config map[string]string) {
	for key := range config {
		if isSensitiveKey(key) {
			config[key] = redactedValue
		}
	}
}

// redact replaces the values of sensitive instance and profile configuration keys.
func (s *Snapshot) redact() {
	for i := range s.Profiles {
		redactConfig(s.Profiles[i].Config)
	}

	for i := range s.Instances {
		redactConfig(s.Instances[i].Config)
		redactConfig(s.Instances[i].ExpandedConfig)
	}
}
