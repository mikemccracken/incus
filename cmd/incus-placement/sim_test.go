package main

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/shared/api"
)

func testSimulator(t *testing.T, scriptlet string) *simulator {
	t.Helper()

	snap, err := loadSnapshot("testdata/cluster.yaml")
	require.NoError(t, err)

	sim, err := newSimulator(snap, scriptlet, "")
	require.NoError(t, err)

	return sim
}

func testScriptlet(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile("testdata/spread.star")
	require.NoError(t, err)

	return string(src)
}

func TestCreate(t *testing.T) {
	opts := createOptions{Project: "default", Name: "db3", Type: "virtual-machine", Profiles: []string{"default", "db"}}

	// Built-in placement picks the member with the fewest instances, skipping group-only members.
	res, err := testSimulator(t, "").create(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, []string{"node3", "node2", "node1"}, res.Candidates)
	require.Equal(t, []exclusion{{Member: "node4", Reason: "scheduler.instance=group and no cluster group was targeted"}}, res.Excluded)
	require.Equal(t, outcomeBuiltin, res.Outcome)
	require.Equal(t, "node3", res.Target)

	// The scriptlet keeps the database instances apart.
	sim := testSimulator(t, testScriptlet(t))
	res, err = sim.create(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, outcomeScriptlet, res.Outcome)
	require.Equal(t, "node3", res.Target)
	require.Equal(t, "node3", sim.instance("default", "db3").Location)

	// A fourth database instance has nowhere left to go.
	opts.Name = "db4"
	res, err = sim.create(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, outcomeRejected, res.Outcome)
	require.Contains(t, res.Error, "No cluster member satisfies the placement rules")

	// An explicit group makes the group-only member a candidate.
	opts.Target = "@gpu"
	res, err = sim.create(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, "node4", res.Target)

	// Any return value is treated as a failure by the daemon.
	sim = testSimulator(t, "def instance_placement(request, candidate_members):\n    return \"nope\"\n")
	res, err = sim.create(context.Background(), createOptions{Project: "default", Name: "c1"})
	require.NoError(t, err)
	require.Equal(t, outcomeRejected, res.Outcome)
	require.Contains(t, res.Error, "unexpected return value")
}

func TestMove(t *testing.T) {
	sim := testSimulator(t, testScriptlet(t))

	// The scriptlet can veto an explicit target.
	res, err := sim.move(context.Background(), "default", "db1", "node2")
	require.NoError(t, err)
	require.Equal(t, []string{"node2"}, res.Candidates)
	require.Equal(t, outcomeRejected, res.Outcome)

	res, err = sim.move(context.Background(), "default", "db1", "node3")
	require.NoError(t, err)
	require.Equal(t, outcomeValidated, res.Outcome)
	require.Equal(t, "node3", sim.instance("default", "db1").Location)

	// A group target lets the scriptlet choose.
	res, err = sim.move(context.Background(), "default", "web1", "@gpu")
	require.NoError(t, err)
	require.Equal(t, outcomeScriptlet, res.Outcome)
	require.Equal(t, "node4", res.Target)
}

func TestEvacuate(t *testing.T) {
	sim := testSimulator(t, testScriptlet(t))

	results, err := sim.evacuate(context.Background(), "node2", false)
	require.NoError(t, err)
	require.Len(t, results, 2)

	// The evacuated member is no longer a candidate and db2 can't join db1.
	require.Equal(t, "db2", results[0].Instance)
	require.NotContains(t, results[0].Candidates, "node2")
	require.Equal(t, "node3", results[0].Target)

	// Instances set to stop aren't placed.
	require.Equal(t, "cache1", results[1].Instance)
	require.Equal(t, outcomeStays, results[1].Outcome)
	require.Equal(t, "node2", sim.instance("default", "cache1").Location)
}

func TestInstanceResources(t *testing.T) {
	root := map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "local"}}

	// Virtual machines get default limits, containers and untyped requests don't.
	res, err := instanceResources(map[string]string{}, root, api.InstanceTypeVM)
	require.NoError(t, err)
	require.Equal(t, uint64(1), res.CPUCores)
	require.Equal(t, uint64(1024*1024*1024), res.MemorySize)
	require.Equal(t, uint64(10*1024*1024*1024), res.RootDiskSize)

	res, err = instanceResources(map[string]string{}, root, api.InstanceTypeAny)
	require.NoError(t, err)
	require.Zero(t, res.CPUCores+res.MemorySize+res.RootDiskSize)

	// Pinned CPUs are counted.
	res, err = instanceResources(map[string]string{"limits.cpu": "0-3,8", "limits.memory": "2GiB"}, root, api.InstanceTypeContainer)
	require.NoError(t, err)
	require.Equal(t, uint64(5), res.CPUCores)
	require.Equal(t, uint64(2*1024*1024*1024), res.MemorySize)
}
