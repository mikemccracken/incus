// Package placement runs the instance placement scriptlet against an abstract source of cluster data.
package placement

import (
	"context"
	"errors"
	"fmt"

	"go.starlark.net/starlark"

	scriptletLoad "github.com/lxc/incus/v7/internal/server/scriptlet/load"
	"github.com/lxc/incus/v7/internal/server/scriptlet/log"
	"github.com/lxc/incus/v7/shared/api"
	apiScriptlet "github.com/lxc/incus/v7/shared/api/scriptlet"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/scriptlet"
)

// Provider supplies the cluster data that the instance placement scriptlet can query.
type Provider interface {
	// LocalMember returns the name of the cluster member running the scriptlet.
	LocalMember() string

	// ClusterMemberResources returns the hardware resources of a cluster member.
	ClusterMemberResources(ctx context.Context, memberName string) (*api.Resources, error)

	// ClusterMemberState returns the state of a cluster member.
	ClusterMemberState(ctx context.Context, memberName string) (*api.ClusterMemberState, error)

	// InstanceResources returns the resources required by the instance being placed.
	InstanceResources(req *apiScriptlet.InstancePlacement) (*apiScriptlet.InstanceResources, error)

	// Instances returns the instances matching the optional project and location filters.
	Instances(ctx context.Context, projectName string, location string) ([]api.Instance, error)

	// InstancesCount returns the number of instances matching the optional project and location filters.
	InstancesCount(ctx context.Context, projectName string, location string, includePending bool) (int, error)

	// ClusterMembers returns the cluster members available for placement, optionally limited to a group.
	ClusterMembers(ctx context.Context, group string) ([]*api.ClusterMember, error)

	// Project returns the named project.
	Project(ctx context.Context, name string) (*api.Project, error)
}

// Run runs the loaded instance placement scriptlet and returns the name of the chosen cluster member.
// An empty name is returned if the scriptlet didn't select a target.
func Run(ctx context.Context, l logger.Logger, req *apiScriptlet.InstancePlacement, candidateMembers []*api.ClusterMember, provider Provider) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	logFunc := log.CreateLogger(l, "Instance placement scriptlet")

	isCandidate := func(memberName string) bool {
		for _, candidateMember := range candidateMembers {
			if candidateMember.ServerName == memberName {
				return true
			}
		}

		return false
	}

	var targetMember string

	setTargetFunc := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var memberName string

		err := starlark.UnpackArgs(b.Name(), args, kwargs, "member_name", &memberName)
		if err != nil {
			return nil, err
		}

		if isCandidate(memberName) {
			targetMember = memberName
		}

		if targetMember == "" {
			l.Error("Instance placement scriptlet set invalid member target", logger.Ctx{"member": memberName})
			return nil, fmt.Errorf("Invalid member name: %s", memberName)
		}

		l.Info("Instance placement scriptlet set member target", logger.Ctx{"member": targetMember})

		return starlark.None, nil
	}

	getClusterMemberResourcesFunc := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var memberName string

		err := starlark.UnpackArgs(b.Name(), args, kwargs, "member_name", &memberName)
		if err != nil {
			return nil, err
		}

		// Only the local member and the candidate members can be queried.
		if memberName != provider.LocalMember() && !isCandidate(memberName) {
			return nil, fmt.Errorf("Invalid member name: %s", memberName)
		}

		res, err := provider.ClusterMemberResources(ctx, memberName)
		if err != nil {
			return nil, err
		}

		rv, err := scriptlet.StarlarkMarshal(res)
		if err != nil {
			return nil, fmt.Errorf("Marshalling cluster member resources for %q failed: %w", memberName, err)
		}

		return rv, nil
	}

	getClusterMemberStateFunc := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var memberName string

		err := starlark.UnpackArgs(b.Name(), args, kwargs, "member_name", &memberName)
		if err != nil {
			return nil, err
		}

		// Only the local member and the candidate members can be queried.
		if memberName != provider.LocalMember() && !isCandidate(memberName) {
			return nil, fmt.Errorf("Invalid member name: %s", memberName)
		}

		memberState, err := provider.ClusterMemberState(ctx, memberName)
		if err != nil {
			return nil, err
		}

		rv, err := scriptlet.StarlarkMarshal(memberState)
		if err != nil {
			return nil, fmt.Errorf("Marshalling cluster member state for %q failed: %w", memberName, err)
		}

		return rv, nil
	}

	getInstanceResourcesFunc := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		res, err := provider.InstanceResources(req)
		if err != nil {
			return nil, fmt.Errorf("Failed to calculate instance resource usage: %w", err)
		}

		rv, err := scriptlet.StarlarkMarshal(res)
		if err != nil {
			return nil, fmt.Errorf("Marshalling instance resources failed: %w", err)
		}

		return rv, nil
	}

	getInstancesFunc := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var project string
		var location string

		err := starlark.UnpackArgs(b.Name(), args, kwargs, "project??", &project, "location??", &location)
		if err != nil {
			return nil, err
		}

		instanceList, err := provider.Instances(ctx, project, location)
		if err != nil {
			return nil, err
		}

		rv, err := scriptlet.StarlarkMarshal(instanceList)
		if err != nil {
			return nil, fmt.Errorf("Marshalling instances failed: %w", err)
		}

		return rv, nil
	}

	getInstancesCountFunc := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var projectName string
		var locationName string
		var includePending bool

		err := starlark.UnpackArgs(b.Name(), args, kwargs, "project??", &projectName, "location??", &locationName, "pending??", &includePending)
		if err != nil {
			return nil, err
		}

		count, err := provider.InstancesCount(ctx, projectName, locationName, includePending)
		if err != nil {
			return nil, err
		}

		rv, err := scriptlet.StarlarkMarshal(count)
		if err != nil {
			return nil, fmt.Errorf("Marshalling instance count failed: %w", err)
		}

		return rv, nil
	}

	getClusterMembersFunc := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var group string

		err := starlark.UnpackArgs(b.Name(), args, kwargs, "group??", &group)
		if err != nil {
			return nil, err
		}

		allMembersInfo, err := provider.ClusterMembers(ctx, group)
		if err != nil {
			return nil, err
		}

		rv, err := scriptlet.StarlarkMarshal(allMembersInfo)
		if err != nil {
			return nil, fmt.Errorf("Marshalling cluster members failed: %w", err)
		}

		return rv, nil
	}

	getProjectFunc := func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var name string

		err := starlark.UnpackArgs(b.Name(), args, kwargs, "name??", &name)
		if err != nil {
			return nil, err
		}

		p, err := provider.Project(ctx, name)
		if err != nil {
			return nil, err
		}

		rv, err := scriptlet.StarlarkMarshal(p)
		if err != nil {
			return nil, fmt.Errorf("Marshalling project failed: %w", err)
		}

		return rv, nil
	}

	// Remember to match the entries in scriptletLoad.InstancePlacementCompile() with this list so Starlark can
	// perform compile time validation of functions used.
	env := starlark.StringDict{
		"log_info":                     starlark.NewBuiltin("log_info", logFunc),
		"log_warn":                     starlark.NewBuiltin("log_warn", logFunc),
		"log_error":                    starlark.NewBuiltin("log_error", logFunc),
		"set_target":                   starlark.NewBuiltin("set_target", setTargetFunc),
		"get_cluster_member_resources": starlark.NewBuiltin("get_cluster_member_resources", getClusterMemberResourcesFunc),
		"get_cluster_member_state":     starlark.NewBuiltin("get_cluster_member_state", getClusterMemberStateFunc),
		"get_instance_resources":       starlark.NewBuiltin("get_instance_resources", getInstanceResourcesFunc),
		"get_instances":                starlark.NewBuiltin("get_instances", getInstancesFunc),
		"get_instances_count":          starlark.NewBuiltin("get_instances_count", getInstancesCountFunc),
		"get_cluster_members":          starlark.NewBuiltin("get_cluster_members", getClusterMembersFunc),
		"get_project":                  starlark.NewBuiltin("get_project", getProjectFunc),
	}

	prog, thread, err := scriptletLoad.InstancePlacementProgram()
	if err != nil {
		return "", err
	}

	go func() {
		<-ctx.Done()
		thread.Cancel("Request finished")
	}()

	globals, err := prog.Init(thread, env)
	if err != nil {
		return "", fmt.Errorf("Failed initializing: %w", err)
	}

	globals.Freeze()

	// Retrieve a global variable from starlark environment.
	instancePlacement := globals["instance_placement"]
	if instancePlacement == nil {
		return "", errors.New("Scriptlet missing instance_placement function")
	}

	rv, err := scriptlet.StarlarkMarshal(req)
	if err != nil {
		return "", fmt.Errorf("Marshalling request failed: %w", err)
	}

	candidateMembersv, err := scriptlet.StarlarkMarshal(candidateMembers)
	if err != nil {
		return "", fmt.Errorf("Marshalling candidate members failed: %w", err)
	}

	// Call starlark function from Go.
	v, err := starlark.Call(thread, instancePlacement, nil, []starlark.Tuple{
		{
			starlark.String("request"),
			rv,
		}, {
			starlark.String("candidate_members"),
			candidateMembersv,
		},
	})
	if err != nil {
		return "", fmt.Errorf("Failed to run: %w", err)
	}

	if v.Type() != "NoneType" {
		return "", fmt.Errorf("Failed with unexpected return value: %v", v)
	}

	return targetMember, nil
}
