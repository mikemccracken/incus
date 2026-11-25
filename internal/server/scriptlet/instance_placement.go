package scriptlet

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/lxc/incus/v7/internal/server/cluster"
	"github.com/lxc/incus/v7/internal/server/db"
	dbCluster "github.com/lxc/incus/v7/internal/server/db/cluster"
	internalInstance "github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/scriptlet/placement"
	"github.com/lxc/incus/v7/internal/server/state"
	"github.com/lxc/incus/v7/shared/api"
	apiScriptlet "github.com/lxc/incus/v7/shared/api/scriptlet"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/resources"
)

// instancePlacementProvider provides the instance placement scriptlet with data from the running cluster.
type instancePlacementProvider struct {
	s                *state.State
	candidateMembers []db.NodeInfo
	leaderAddress    string
}

// LocalMember returns the name of the local cluster member.
func (p *instancePlacementProvider) LocalMember() string {
	return p.s.ServerName
}

// candidateMember returns the candidate member with the given name.
func (p *instancePlacementProvider) candidateMember(memberName string) (*db.NodeInfo, error) {
	for i := range p.candidateMembers {
		if p.candidateMembers[i].Name == memberName {
			return &p.candidateMembers[i], nil
		}
	}

	return nil, fmt.Errorf("Invalid member name: %s", memberName)
}

// ClusterMemberResources returns the hardware resources of a cluster member.
func (p *instancePlacementProvider) ClusterMemberResources(ctx context.Context, memberName string) (*api.Resources, error) {
	// Get the local resource usage.
	if memberName == p.s.ServerName {
		return resources.GetResources()
	}

	// Get remote member resource usage.
	targetMember, err := p.candidateMember(memberName)
	if err != nil {
		return nil, err
	}

	client, err := cluster.Connect(targetMember.Address, p.s.Endpoints.NetworkCert(), p.s.ServerCert(), nil, true)
	if err != nil {
		return nil, err
	}

	return client.GetServerResources()
}

// ClusterMemberState returns the state of a cluster member.
func (p *instancePlacementProvider) ClusterMemberState(ctx context.Context, memberName string) (*api.ClusterMemberState, error) {
	// Get the local member state.
	if memberName == p.s.ServerName {
		return cluster.MemberState(ctx, p.s, memberName)
	}

	// Get remote member state.
	targetMember, err := p.candidateMember(memberName)
	if err != nil {
		return nil, err
	}

	client, err := cluster.Connect(targetMember.Address, p.s.Endpoints.NetworkCert(), p.s.ServerCert(), nil, true)
	if err != nil {
		return nil, err
	}

	memberState, _, err := client.GetClusterMemberState(memberName)
	if err != nil {
		return nil, err
	}

	return memberState, nil
}

// InstanceResources returns the resources required by the instance being placed.
func (p *instancePlacementProvider) InstanceResources(req *apiScriptlet.InstancePlacement) (*apiScriptlet.InstanceResources, error) {
	usageCPU, usageMemory, usageDisk, err := internalInstance.ResourceUsage(req.Config, req.Devices, req.Type)
	if err != nil {
		return nil, err
	}

	return &apiScriptlet.InstanceResources{
		CPUCores:     uint64(usageCPU),
		MemorySize:   uint64(usageMemory),
		RootDiskSize: uint64(usageDisk),
	}, nil
}

// Instances returns the instances matching the optional project and location filters.
func (p *instancePlacementProvider) Instances(ctx context.Context, project string, location string) ([]api.Instance, error) {
	instanceList := []api.Instance{}

	err := p.s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		var objects []dbCluster.Instance
		var err error

		if project != "" || location != "" {
			// Prepare a filter.
			filter := dbCluster.InstanceFilter{}

			if project != "" {
				filter.Project = &project
			}

			if location != "" {
				filter.Node = &location
			}

			// Get instances based on Project and/or Location filters.
			objects, err = dbCluster.GetInstances(ctx, tx.Tx(), filter)
			if err != nil {
				return err
			}
		} else {
			// Get all instances.
			objects, err = dbCluster.GetInstances(ctx, tx.Tx())
			if err != nil {
				return err
			}
		}

		objectIDs := make([]int, 0, len(objects))
		for _, obj := range objects {
			objectIDs = append(objectIDs, obj.ID)
		}

		// Load everything in bulk, per instance queries are too slow on a large cluster.
		objectConfigs, err := dbCluster.GetConfig(ctx, tx.Tx(), "instances", "instance", dbCluster.ConfigFilter{ReferenceID: objectIDs})
		if err != nil {
			return err
		}

		objectDevices, err := dbCluster.GetDevices(ctx, tx.Tx(), "instances", "instance", dbCluster.DeviceFilter{ReferenceID: objectIDs})
		if err != nil {
			return err
		}

		objectProfiles, err := dbCluster.GetAllInstanceProfiles(ctx, tx.Tx())
		if err != nil {
			return err
		}

		profilesByID := map[int]dbCluster.Profile{}
		for _, obj := range objects {
			for _, profile := range objectProfiles[obj.ID] {
				profilesByID[profile.ID] = profile
			}
		}

		profiles := make([]dbCluster.Profile, 0, len(profilesByID))
		for _, profile := range profilesByID {
			profiles = append(profiles, profile)
		}

		profileConfigs, err := dbCluster.GetReferencedProfileConfigs(ctx, tx.Tx(), profiles)
		if err != nil {
			return err
		}

		profileDevices, err := dbCluster.GetReferencedProfileDevices(ctx, tx.Tx(), profiles)
		if err != nil {
			return err
		}

		// Convert the []Instances into []api.Instances.
		for _, obj := range objects {
			instance, err := obj.ToAPI(ctx, tx.Tx(), objectConfigs, objectDevices, objectProfiles, profileConfigs, profileDevices)
			if err != nil {
				return err
			}

			instanceList = append(instanceList, *instance)
		}

		// Account for moves that have been decided but not completed yet.
		for _, pending := range instancePlacementPending(project) {
			isPlacement := func(inst api.Instance) bool {
				return inst.Project == pending.project && inst.Name == pending.name
			}

			if location == "" {
				for i := range instanceList {
					if isPlacement(instanceList[i]) {
						instanceList[i].Location = pending.target
					}
				}

				continue
			}

			if pending.target != location {
				instanceList = slices.DeleteFunc(instanceList, isPlacement)
				continue
			}

			obj, err := dbCluster.GetInstance(ctx, tx.Tx(), pending.project, pending.name)
			if api.StatusErrorCheck(err, http.StatusNotFound) {
				continue
			}

			if err != nil {
				return err
			}

			devices, err := dbCluster.GetDevices(ctx, tx.Tx(), "instances", "instance", dbCluster.DeviceFilter{ReferenceID: []int{obj.ID}})
			if err != nil {
				return err
			}

			instance, err := obj.ToAPI(ctx, tx.Tx(), nil, devices, nil, nil, nil)
			if err != nil {
				return err
			}

			instance.Location = pending.target
			instanceList = append(instanceList, *instance)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return instanceList, nil
}

// InstancesCount returns the number of instances matching the optional project and location filters.
func (p *instancePlacementProvider) InstancesCount(ctx context.Context, projectName string, locationName string, includePending bool) (int, error) {
	var count int

	err := p.s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		count, err = tx.GetInstancesCount(ctx, projectName, locationName, includePending)

		return err
	})
	if err != nil {
		return -1, err
	}

	// Account for moves that have been decided but not completed yet.
	if locationName != "" {
		for _, pending := range instancePlacementPending(projectName) {
			if pending.target == locationName {
				count++
			} else if pending.source == locationName {
				count--
			}
		}
	}

	return count, nil
}

// ClusterMembers returns the cluster members available for placement, optionally limited to a group.
func (p *instancePlacementProvider) ClusterMembers(ctx context.Context, group string) ([]*api.ClusterMember, error) {
	var allMembers []db.NodeInfo

	err := p.s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		var err error

		allMembers, err = tx.GetNodes(ctx)
		if err != nil {
			return err
		}

		allMembers, err = tx.GetCandidateMembers(ctx, allMembers, nil, group, nil, p.s.GlobalConfig.OfflineThreshold())
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return p.membersToAPI(ctx, allMembers)
}

// Project returns the named project.
func (p *instancePlacementProvider) Project(ctx context.Context, name string) (*api.Project, error) {
	var apiProject *api.Project

	err := p.s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		dbProject, err := dbCluster.GetProject(ctx, tx.Tx(), name)
		if err != nil {
			return err
		}

		apiProject, err = dbProject.ToAPI(ctx, tx.Tx())
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return apiProject, nil
}

// membersToAPI converts cluster member database records to their API representation.
func (p *instancePlacementProvider) membersToAPI(ctx context.Context, members []db.NodeInfo) ([]*api.ClusterMember, error) {
	var raftNodes []db.RaftNode

	err := p.s.DB.Node.Transaction(ctx, func(ctx context.Context, tx *db.NodeTx) error {
		var err error

		raftNodes, err = tx.GetRaftNodes(ctx)
		if err != nil {
			return fmt.Errorf("Failed loading RAFT nodes: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	membersInfo := make([]*api.ClusterMember, 0, len(members))
	err = p.s.DB.Cluster.Transaction(ctx, func(ctx context.Context, tx *db.ClusterTx) error {
		failureDomains, err := tx.GetFailureDomainsNames(ctx)
		if err != nil {
			return fmt.Errorf("Failed loading failure domains names: %w", err)
		}

		memberFailureDomains, err := tx.GetNodesFailureDomains(ctx)
		if err != nil {
			return fmt.Errorf("Failed loading member failure domains: %w", err)
		}

		maxVersion, err := tx.GetNodeMaxVersion(ctx)
		if err != nil {
			return fmt.Errorf("Failed getting max member version: %w", err)
		}

		args := db.NodeInfoArgs{
			LeaderAddress:        p.leaderAddress,
			FailureDomains:       failureDomains,
			MemberFailureDomains: memberFailureDomains,
			OfflineThreshold:     p.s.GlobalConfig.OfflineThreshold(),
			MaxMemberVersion:     maxVersion,
			RaftNodes:            raftNodes,
		}

		for i := range members {
			memberInfo, err := db.APINodeInfo{NodeInfo: members[i]}.ToAPI(ctx, tx, args)
			if err != nil {
				return err
			}

			membersInfo = append(membersInfo, memberInfo)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return membersInfo, nil
}

// InstancePlacementRun runs the instance placement scriptlet and returns the chosen cluster member target.
func InstancePlacementRun(ctx context.Context, l logger.Logger, s *state.State, req *apiScriptlet.InstancePlacement, candidateMembers []db.NodeInfo, leaderAddress string) (*db.NodeInfo, error) {
	provider := &instancePlacementProvider{
		s:                s,
		candidateMembers: candidateMembers,
		leaderAddress:    leaderAddress,
	}

	candidateMembersInfo, err := provider.membersToAPI(ctx, candidateMembers)
	if err != nil {
		return nil, err
	}

	targetMemberName, err := placement.Run(ctx, l, req, candidateMembersInfo, provider)
	if err != nil {
		return nil, err
	}

	var targetMember *db.NodeInfo
	for i := range candidateMembers {
		if candidateMembers[i].Name == targetMemberName {
			targetMember = &candidateMembers[i]
			break
		}
	}

	return targetMember, nil
}
