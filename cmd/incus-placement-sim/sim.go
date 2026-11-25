package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	internalInstance "github.com/lxc/incus/v7/internal/instance"
	"github.com/lxc/incus/v7/internal/server/instance/drivers/qemudefault"
	scriptletLoad "github.com/lxc/incus/v7/internal/server/scriptlet/load"
	"github.com/lxc/incus/v7/internal/server/scriptlet/placement"
	"github.com/lxc/incus/v7/shared/api"
	apiScriptlet "github.com/lxc/incus/v7/shared/api/scriptlet"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/osarch"
	"github.com/lxc/incus/v7/shared/units"
	"github.com/lxc/incus/v7/shared/util"
	"github.com/lxc/incus/v7/shared/validate"
)

// defaultBlockSize mirrors the default root disk size of virtual machines (storage drivers DefaultBlockSize).
const defaultBlockSize = "10GiB"

// evacuationTimeout mirrors the scriptlet timeout used by the daemon during evacuation.
const evacuationTimeout = 10 * time.Second

// Possible placement outcomes.
const (
	outcomeScriptlet = "scriptlet"
	outcomeBuiltin   = "builtin"
	outcomeValidated = "validated"
	outcomeRejected  = "rejected"
	outcomeStays     = "stays"
)

// Kinds of trace entries.
const (
	traceCall = "call"
	traceLog  = "log"
)

// traceEntry is a query made or a message logged by the scriptlet, in the order they happened.
type traceEntry struct {
	Kind     string            `json:"kind"`
	Level    string            `json:"level,omitempty"`
	Message  string            `json:"message"`
	Function string            `json:"function,omitempty"`
	Args     map[string]any    `json:"args,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
	Member   string            `json:"member,omitempty"`
}

// Reasons for a cluster member not to be a placement candidate.
const (
	excludedStatus        = "status"
	excludedManual        = "scheduler_manual"
	excludedGroupOnly     = "scheduler_group"
	excludedClusterGroup  = "cluster_group"
	excludedProjectGroups = "project_cluster_groups"
	excludedArchitecture  = "architecture"
)

// exclusion records why a cluster member wasn't a placement candidate.
type exclusion struct {
	Member string `json:"member"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// instanceUsage is what an instance is limited to.
type instanceUsage struct {
	CPUs     uint64 `json:"cpus"`
	Memory   uint64 `json:"memory"`
	RootDisk uint64 `json:"root_disk"`
}

// memberState is the status of a cluster member and what its instances add up to.
type memberState struct {
	Status          string `json:"status"`
	Instances       int    `json:"instances"`
	CommittedCPUs   uint64 `json:"committed_cpus"`
	CommittedMemory uint64 `json:"committed_memory"`
}

// instanceState is where an instance is and what it is limited to.
type instanceState struct {
	Project  string `json:"project"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Location string `json:"location"`
	Status   string `json:"status"`

	instanceUsage
}

// clusterState is the changing part of a cluster.
type clusterState struct {
	Members   map[string]memberState `json:"members"`
	Instances []instanceState        `json:"instances"`
}

// action is something the simulator was asked to do, along with the placements it led to.
type action struct {
	Type    string
	Params  map[string]any
	Results []*result
	Error   string
}

// result describes what happened to a single instance placement.
type result struct {
	Reason         string                 `json:"reason"`
	Project        string                 `json:"project"`
	Instance       string                 `json:"instance"`
	InstanceType   string                 `json:"instance_type"`
	Resources      instanceUsage          `json:"resources"`
	Source         string                 `json:"source,omitempty"`
	Candidates     []string               `json:"candidates"`
	InstanceCounts map[string]int         `json:"instance_counts,omitempty"`
	Excluded       []exclusion            `json:"excluded,omitempty"`
	Outcome        string                 `json:"outcome"`
	Target         string                 `json:"target,omitempty"`
	Detail         string                 `json:"detail,omitempty"`
	Error          string                 `json:"error,omitempty"`
	Trace          []traceEntry           `json:"trace,omitempty"`
	Duration       string                 `json:"duration,omitempty"`
	DurationMicros int64                  `json:"duration_us,omitempty"`
	MembersAfter   map[string]memberState `json:"members_after,omitempty"`
}

// describe records the kind of instance being placed and what it is limited to.
func (res *result) describe(instType string, config map[string]string, devices map[string]map[string]string) {
	if instType == "" {
		instType = string(api.InstanceTypeContainer)
	}

	res.InstanceType = instType
	res.Resources = usageOf(instType, config, devices)
}

// usageOf returns what an instance is limited to, leaving out limits which can't be parsed.
func usageOf(instType string, config map[string]string, devices map[string]map[string]string) instanceUsage {
	res, err := instanceResources(config, devices, api.InstanceType(instType))
	if err != nil {
		return instanceUsage{}
	}

	return instanceUsage{CPUs: res.CPUCores, Memory: res.MemorySize, RootDisk: res.RootDiskSize}
}

// recorder is a logger which records the scriptlet messages in a result.
type recorder struct {
	res *result
}

func (r *recorder) record(level string, msg string, args []logger.Ctx) {
	// The scriptlet picking a member is reported as a log message, show it as the call it was.
	if msg == "Instance placement scriptlet set member target" && len(args) == 1 {
		member := fmt.Sprint(args[0]["member"])
		r.res.Trace = append(r.res.Trace, traceEntry{
			Kind:     traceCall,
			Message:  fmt.Sprintf("set_target(member_name=%q)", member),
			Function: "set_target",
			Args:     map[string]any{"member_name": member},
			Member:   member,
		})

		return
	}

	msg = strings.TrimPrefix(msg, "Instance placement scriptlet: ")

	entry := traceEntry{Kind: traceLog, Level: level}
	for _, ctx := range args {
		keys := slices.Sorted(maps.Keys(ctx))
		for _, key := range keys {
			msg = fmt.Sprintf("%s %s=%v", msg, key, ctx[key])

			if entry.Fields == nil {
				entry.Fields = map[string]string{}
			}

			entry.Fields[key] = fmt.Sprint(ctx[key])
		}
	}

	entry.Message = msg
	entry.Member = entry.Fields["member"]

	r.res.Trace = append(r.res.Trace, entry)
}

// Panic records a message.
func (r *recorder) Panic(msg string, args ...logger.Ctx) { r.record("panic", msg, args) }

// Fatal records a message.
func (r *recorder) Fatal(msg string, args ...logger.Ctx) { r.record("fatal", msg, args) }

// Error records a message.
func (r *recorder) Error(msg string, args ...logger.Ctx) { r.record("error", msg, args) }

// Warn records a message.
func (r *recorder) Warn(msg string, args ...logger.Ctx) { r.record("warn", msg, args) }

// Info records a message.
func (r *recorder) Info(msg string, args ...logger.Ctx) { r.record("info", msg, args) }

// Debug records a message.
func (r *recorder) Debug(msg string, args ...logger.Ctx) { r.record("debug", msg, args) }

// Trace records a message.
func (r *recorder) Trace(msg string, args ...logger.Ctx) { r.record("trace", msg, args) }

// WarnOnErrorExcept records a message if an error is set.
func (r *recorder) WarnOnErrorExcept(err error, _ []error, msg string, args ...logger.Ctx) {
	if err != nil {
		r.record("warn", msg, args)
	}
}

// AddContext returns the same logger.
func (r *recorder) AddContext(logger.Ctx) logger.Logger { return r }

// simulator runs placement requests against a cluster snapshot.
type simulator struct {
	snap         *Snapshot
	localMember  string
	hasScriptlet bool

	// What the cluster looked like to begin with and what was done to it since.
	initial clusterState
	actions []action
}

// newSimulator prepares a simulator and loads the scriptlet (if any).
func newSimulator(snap *Snapshot, scriptletSrc string, localMember string) (*simulator, error) {
	s := &simulator{snap: snap, localMember: localMember}

	if s.localMember == "" {
		s.localMember = snap.Members[0].ServerName
	}

	if s.member(s.localMember) == nil {
		return nil, fmt.Errorf("Cluster member %q not found", s.localMember)
	}

	if scriptletSrc != "" {
		err := scriptletLoad.InstancePlacementValidate(scriptletSrc)
		if err != nil {
			return nil, fmt.Errorf("Invalid instance placement scriptlet: %w", err)
		}

		s.hasScriptlet = true
	}

	err := scriptletLoad.InstancePlacementSet(scriptletSrc)
	if err != nil {
		return nil, fmt.Errorf("Failed loading instance placement scriptlet: %w", err)
	}

	s.initial = s.state()

	return s, nil
}

// memberStates returns the status of every cluster member and what its instances add up to.
func (s *simulator) memberStates() map[string]memberState {
	usage := s.usage()

	states := make(map[string]memberState, len(s.snap.Members))
	for _, member := range s.snap.Members {
		states[member.ServerName] = memberState{
			Status:          member.Status,
			Instances:       usage[member.ServerName].Instances,
			CommittedCPUs:   usage[member.ServerName].CPUs,
			CommittedMemory: usage[member.ServerName].Memory,
		}
	}

	return states
}

// state returns the changing part of the cluster as it currently is.
func (s *simulator) state() clusterState {
	state := clusterState{Members: s.memberStates(), Instances: make([]instanceState, 0, len(s.snap.Instances))}
	for _, inst := range s.snap.Instances {
		state.Instances = append(state.Instances, instanceState{
			Project:       inst.Project,
			Name:          inst.Name,
			Type:          inst.Type,
			Location:      inst.Location,
			Status:        inst.Status,
			instanceUsage: usageOf(inst.Type, inst.ExpandedConfig, inst.ExpandedDevices),
		})
	}

	return state
}

// record logs an action, along with the state each of its placements left the cluster in.
func (s *simulator) record(actionType string, params map[string]any, err error, results ...*result) {
	act := action{Type: actionType, Params: params}
	if err != nil {
		act.Error = err.Error()
	}

	for _, res := range results {
		if res == nil {
			continue
		}

		if res.MembersAfter == nil {
			res.MembersAfter = s.memberStates()
		}

		act.Results = append(act.Results, res)
	}

	s.actions = append(s.actions, act)
}

// member returns the named cluster member.
func (s *simulator) member(name string) *SnapshotMember {
	for i := range s.snap.Members {
		if s.snap.Members[i].ServerName == name {
			return &s.snap.Members[i]
		}
	}

	return nil
}

// project returns the named project.
func (s *simulator) project(name string) (*api.Project, error) {
	for i := range s.snap.Projects {
		if s.snap.Projects[i].Name == name {
			return &s.snap.Projects[i], nil
		}
	}

	names := make([]string, 0, len(s.snap.Projects))
	for _, p := range s.snap.Projects {
		names = append(names, p.Name)
	}

	return nil, fmt.Errorf("Project %q not found, snapshot has: %s", name, strings.Join(names, ", "))
}

// instance returns the named instance.
func (s *simulator) instance(projectName string, name string) *api.Instance {
	for i := range s.snap.Instances {
		if s.snap.Instances[i].Project == projectName && s.snap.Instances[i].Name == name {
			return &s.snap.Instances[i]
		}
	}

	return nil
}

// profiles returns the named profiles as seen from the given project.
func (s *simulator) profiles(p *api.Project, names []string) ([]api.Profile, error) {
	// Profiles come from the default project unless the project has its own.
	profileProject := api.ProjectDefaultName
	if util.IsTrue(p.Config["features.profiles"]) {
		profileProject = p.Name
	}

	profiles := make([]api.Profile, 0, len(names))
	for _, name := range names {
		found := false
		for _, profile := range s.snap.Profiles {
			if profile.Project == profileProject && profile.Name == name {
				profiles = append(profiles, profile)
				found = true
				break
			}
		}

		if !found {
			return nil, fmt.Errorf("Requested profile %q doesn't exist", name)
		}
	}

	return profiles, nil
}

// instancesCount returns the number of instances matching the optional project and location filters.
func (s *simulator) instancesCount(projectName string, location string) int {
	count := 0
	for _, inst := range s.snap.Instances {
		if projectName != "" && inst.Project != projectName {
			continue
		}

		if location != "" && inst.Location != location {
			continue
		}

		count++
	}

	return count
}

// memberUsage is what the instances placed on a cluster member add up to.
type memberUsage struct {
	Instances int
	CPUs      uint64
	Memory    uint64
}

// usage returns the instance count and the sum of the instance limits of each cluster member.
func (s *simulator) usage() map[string]memberUsage {
	result := map[string]memberUsage{}
	for _, member := range s.snap.Members {
		result[member.ServerName] = memberUsage{}
	}

	for i := range s.snap.Instances {
		inst := &s.snap.Instances[i]

		usage := result[inst.Location]
		usage.Instances++

		// Instances with limits which can't be parsed only count towards the instance total.
		res, err := instanceResources(inst.ExpandedConfig, inst.ExpandedDevices, api.InstanceType(inst.Type))
		if err == nil {
			usage.CPUs += res.CPUCores
			usage.Memory += res.MemorySize
		}

		result[inst.Location] = usage
	}

	return result
}

// groupExists returns whether the named cluster group exists.
func (s *simulator) groupExists(name string) bool {
	for _, group := range s.snap.Groups {
		if group.Name == name {
			return true
		}
	}

	return false
}

// restrictedClusterGroups mirrors project.GetRestrictedClusterGroups.
func restrictedClusterGroups(p *api.Project) []string {
	if util.IsFalseOrEmpty(p.Config["restricted"]) {
		return nil
	}

	return util.SplitNTrimSpace(p.Config["restricted.cluster.groups"], ",", -1, true)
}

// noCandidateError mirrors project.NoCandidateMemberError.
func noCandidateError(p *api.Project) error {
	allowedGroups := restrictedClusterGroups(p)
	if len(allowedGroups) > 1 {
		return fmt.Errorf("Couldn't find a cluster member for the instance, target one of the project's cluster groups: %s", strings.Join(allowedGroups, ", "))
	}

	return errors.New("Couldn't find a cluster member for the instance")
}

// isSchedulable mirrors the member state check of db.GetCandidateMembers (created and online).
func isSchedulable(member *SnapshotMember) bool {
	return member.Status == "Online" || member.Status == "Blocked"
}

// architectureNames returns a readable list of architectures.
func architectureNames(architectures []int) string {
	names := make([]string, 0, len(architectures))
	for _, arch := range architectures {
		name, err := osarch.ArchitectureName(arch)
		if err != nil {
			name = strconv.Itoa(arch)
		}

		names = append(names, name)
	}

	return strings.Join(names, " or ")
}

// exclusionReason mirrors the member filter of db.GetCandidateMembers, returning "" for candidates.
func (s *simulator) exclusionReason(member *SnapshotMember, architectures []int, group string, allowedGroups []string) (string, string, error) {
	// Skip pending, evacuated or offline members.
	if !isSchedulable(member) {
		return excludedStatus, "status is " + member.Status, nil
	}

	// Skip manually targeted members.
	if member.Config["scheduler.instance"] == "manual" {
		return excludedManual, "scheduler.instance=manual", nil
	}

	// Skip group-only members if targeted cluster group doesn't match.
	if member.Config["scheduler.instance"] == "group" && !slices.Contains(member.Groups, group) {
		if group == "" {
			return excludedGroupOnly, "scheduler.instance=group and no cluster group was targeted", nil
		}

		return excludedGroupOnly, fmt.Sprintf("scheduler.instance=group and not in cluster group %q", group), nil
	}

	// Skip if a group is requested and member isn't part of it.
	if group != "" && !slices.Contains(member.Groups, group) {
		return excludedClusterGroup, fmt.Sprintf("not in cluster group %q", group), nil
	}

	// Skip if working with a restricted set of cluster groups and member isn't part of any.
	if allowedGroups != nil {
		found := false
		for _, allowedGroup := range allowedGroups {
			if !s.groupExists(allowedGroup) {
				return "", "", fmt.Errorf("Cluster group %q doesn't exist", allowedGroup)
			}

			if slices.Contains(member.Groups, allowedGroup) {
				found = true
				break
			}
		}

		if !found {
			return excludedProjectGroups, "not in a cluster group allowed by the project (" + strings.Join(allowedGroups, ", ") + ")", nil
		}
	}

	// Consider target architectures if specified.
	if architectures != nil {
		memberArch, err := osarch.ArchitectureID(member.Architecture)
		if err != nil {
			return "", "", err
		}

		personalities, err := osarch.ArchitecturePersonalities(memberArch)
		if err != nil {
			return "", "", err
		}

		supported := append([]int{memberArch}, personalities...)
		for _, arch := range supported {
			if slices.Contains(architectures, arch) {
				return "", "", nil
			}
		}

		return excludedArchitecture, fmt.Sprintf("architecture is %s, instance needs %s", member.Architecture, architectureNames(architectures)), nil
	}

	return "", "", nil
}

// candidates mirrors db.GetCandidateMembers and also reports why the other members were left out.
func (s *simulator) candidates(architectures []int, group string, allowedGroups []string) ([]*api.ClusterMember, []exclusion, error) {
	var candidateMembers []*api.ClusterMember
	var excluded []exclusion

	for i := range s.snap.Members {
		member := &s.snap.Members[i]

		code, reason, err := s.exclusionReason(member, architectures, group, allowedGroups)
		if err != nil {
			return nil, nil, err
		}

		if code != "" {
			excluded = append(excluded, exclusion{Member: member.ServerName, Code: code, Reason: reason})
			continue
		}

		candidateMembers = append(candidateMembers, &member.ClusterMember)
	}

	// Sort by instance count using the same (unstable) sort as the daemon.
	sort.Slice(candidateMembers, func(i int, j int) bool {
		return s.instancesCount("", candidateMembers[i].ServerName) < s.instancesCount("", candidateMembers[j].ServerName)
	})

	return candidateMembers, excluded, nil
}

// checkTarget mirrors project.CheckTarget (without the authorization check).
func (s *simulator) checkTarget(p *api.Project, target string) (*SnapshotMember, string, error) {
	groupName, isGroup := strings.CutPrefix(target, "@")
	allowedGroups := restrictedClusterGroups(p)

	// A project restricted to a single cluster group targets it by default.
	if target == "" && len(allowedGroups) == 1 {
		groupName = allowedGroups[0]
	}

	if !isGroup && target != "" {
		member := s.member(target)
		if member == nil {
			return nil, "", fmt.Errorf("Cluster member %q not found", target)
		}

		if len(allowedGroups) > 0 {
			allowed := slices.ContainsFunc(member.Groups, func(name string) bool {
				return slices.Contains(allowedGroups, name)
			})

			if !allowed {
				return nil, "", fmt.Errorf("Project isn't allowed to use this cluster member: %q", target)
			}
		}

		return member, "", nil
	}

	if groupName != "" {
		if len(allowedGroups) > 0 && !slices.Contains(allowedGroups, groupName) {
			return nil, "", fmt.Errorf("Project isn't allowed to use this cluster group: %q", groupName)
		}

		if !s.groupExists(groupName) {
			return nil, "", fmt.Errorf("Cluster group %q doesn't exist", groupName)
		}

		return nil, groupName, nil
	}

	return nil, "", nil
}

// simProvider serves the scriptlet from the snapshot and records the queries made.
type simProvider struct {
	sim *simulator
	res *result
}

// callArg is an argument of a function called by the scriptlet.
type callArg struct {
	name  string
	value any
}

// call records a function called by the scriptlet, noting which cluster member it was about (if any).
func (p *simProvider) call(function string, args ...callArg) {
	entry := traceEntry{Kind: traceCall, Function: function}

	shown := make([]string, 0, len(args))
	for _, arg := range args {
		if entry.Args == nil {
			entry.Args = map[string]any{}
		}

		entry.Args[arg.name] = arg.value

		text, isText := arg.value.(string)
		if !isText {
			shown = append(shown, fmt.Sprintf("%s=%v", arg.name, arg.value))
			continue
		}

		shown = append(shown, fmt.Sprintf("%s=%q", arg.name, text))

		if arg.name == "member_name" || arg.name == "location" {
			entry.Member = text
		}
	}

	entry.Message = function + "(" + strings.Join(shown, ", ") + ")"

	p.res.Trace = append(p.res.Trace, entry)
}

// LocalMember returns the member simulated as running the scriptlet.
func (p *simProvider) LocalMember() string {
	return p.sim.localMember
}

// ClusterMemberResources returns the captured resources of a cluster member.
func (p *simProvider) ClusterMemberResources(_ context.Context, memberName string) (*api.Resources, error) {
	p.call("get_cluster_member_resources", callArg{"member_name", memberName})

	member := p.sim.member(memberName)
	if member == nil || member.Resources == nil {
		return nil, fmt.Errorf("No resources captured for cluster member %q", memberName)
	}

	return member.Resources, nil
}

// ClusterMemberState returns the captured state of a cluster member.
func (p *simProvider) ClusterMemberState(_ context.Context, memberName string) (*api.ClusterMemberState, error) {
	p.call("get_cluster_member_state", callArg{"member_name", memberName})

	member := p.sim.member(memberName)
	if member == nil || member.State == nil {
		return nil, fmt.Errorf("No state captured for cluster member %q", memberName)
	}

	return member.State, nil
}

// InstanceResources returns the resources required by the instance being placed.
func (p *simProvider) InstanceResources(req *apiScriptlet.InstancePlacement) (*apiScriptlet.InstanceResources, error) {
	p.call("get_instance_resources")

	return instanceResources(req.Config, req.Devices, req.Type)
}

// Instances returns the instances matching the optional project and location filters.
func (p *simProvider) Instances(_ context.Context, projectName string, location string) ([]api.Instance, error) {
	p.call("get_instances", callArg{"project", projectName}, callArg{"location", location})

	instances := []api.Instance{}
	for _, inst := range p.sim.snap.Instances {
		if projectName != "" && inst.Project != projectName {
			continue
		}

		if location != "" && inst.Location != location {
			continue
		}

		instances = append(instances, inst)
	}

	return instances, nil
}

// InstancesCount returns the number of instances matching the optional project and location filters.
func (p *simProvider) InstancesCount(_ context.Context, projectName string, location string, includePending bool) (int, error) {
	p.call("get_instances_count", callArg{"project", projectName}, callArg{"location", location}, callArg{"pending", includePending})

	return p.sim.instancesCount(projectName, location), nil
}

// ClusterMembers returns the cluster members available for placement, optionally limited to a group.
func (p *simProvider) ClusterMembers(_ context.Context, group string) ([]*api.ClusterMember, error) {
	p.call("get_cluster_members", callArg{"group", group})

	members, _, err := p.sim.candidates(nil, group, nil)

	return members, err
}

// Project returns the named project.
func (p *simProvider) Project(_ context.Context, name string) (*api.Project, error) {
	p.call("get_project", callArg{"name", name})

	return p.sim.project(name)
}

// parseCpuset mirrors resources.ParseCpuset, which is only available on Linux.
func parseCpuset(cpu string) ([]int64, error) {
	res := []int64{}
	for chunk := range strings.SplitSeq(cpu, ",") {
		low, high, isRange := strings.Cut(chunk, "-")
		if !isRange {
			high = low
		}

		lowNr, err := strconv.ParseInt(low, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Invalid cpuset value %q: %w", cpu, err)
		}

		highNr, err := strconv.ParseInt(high, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Invalid cpuset value %q: %w", cpu, err)
		}

		for i := lowNr; i <= highNr; i++ {
			res = append(res, i)
		}
	}

	return res, nil
}

// cpuUsage mirrors instance.CPUUsage from the daemon.
func cpuUsage(config map[string]string, instType api.InstanceType) (int64, error) {
	limitsCPU := config["limits.cpu"]
	if limitsCPU == "" {
		if instType == api.InstanceTypeVM {
			return qemudefault.CPUCores, nil
		}

		return 0, nil
	}

	// Check if using shared CPU limits.
	usage, err := strconv.ParseInt(limitsCPU, 10, 64)
	if err != nil && instType == api.InstanceTypeVM && strings.Contains(limitsCPU, "=") {
		// Or compute the total from an explicit CPU topology.
		sockets, cores, threads, err := validate.ParseCPUTopology(limitsCPU)
		if err != nil {
			return -1, fmt.Errorf("Failed parsing instance resources limits.cpu: %w", err)
		}

		return int64(sockets * cores * threads), nil
	} else if err != nil {
		// Or get count of pinned CPUs.
		pinnedCPUs, err := parseCpuset(limitsCPU)
		if err != nil {
			return -1, fmt.Errorf("Failed parsing instance resources limits.cpu: %w", err)
		}

		return int64(len(pinnedCPUs)), nil
	}

	return usage, nil
}

// instanceResources mirrors instance.ResourceUsage from the daemon.
func instanceResources(config map[string]string, devices map[string]map[string]string, instType api.InstanceType) (*apiScriptlet.InstanceResources, error) {
	res := &apiScriptlet.InstanceResources{}

	usage, err := cpuUsage(config, instType)
	if err != nil {
		return nil, err
	}

	res.CPUCores = uint64(usage)

	// Apply VM memory limit defaults if not specified.
	memoryLimit := config["limits.memory"]
	if instType == api.InstanceTypeVM && memoryLimit == "" {
		memoryLimit = qemudefault.MemSize
	}

	if memoryLimit != "" {
		size, err := units.ParseByteSizeString(memoryLimit)
		if err != nil {
			return nil, fmt.Errorf("Failed parsing instance resources limits.memory: %w", err)
		}

		res.MemorySize = uint64(size)
	}

	// Apply VM root disk size defaults if not specified.
	_, rootDisk, err := internalInstance.GetRootDiskDevice(devices)
	if err == nil {
		rootDiskSize := rootDisk["size"]
		if instType == api.InstanceTypeVM && rootDiskSize == "" {
			rootDiskSize = defaultBlockSize
		}

		if rootDiskSize != "" {
			size, err := units.ParseByteSizeString(rootDiskSize)
			if err != nil {
				return nil, fmt.Errorf("Failed parsing instance resources root disk size: %w", err)
			}

			res.RootDiskSize = uint64(size)
		}
	}

	return res, nil
}

// expandConfig mirrors db.ExpandInstanceConfig.
func expandConfig(config map[string]string, profiles []api.Profile) map[string]string {
	expanded := map[string]string{}
	for _, profile := range profiles {
		maps.Copy(expanded, profile.Config)
	}

	maps.Copy(expanded, config)

	return expanded
}

// expandDevices mirrors db.ExpandInstanceDevices.
func expandDevices(devices map[string]map[string]string, profiles []api.Profile) map[string]map[string]string {
	expanded := map[string]map[string]string{}
	for _, profile := range profiles {
		for name, device := range profile.Devices {
			expanded[name] = maps.Clone(device)
		}
	}

	for name, device := range devices {
		expanded[name] = maps.Clone(device)
	}

	return expanded
}

// memberNames returns the names of the given members.
func memberNames(members []*api.ClusterMember) []string {
	names := make([]string, 0, len(members))
	for _, member := range members {
		names = append(names, member.ServerName)
	}

	return names
}

// place runs the scriptlet for a request and returns the member it selected (if any).
func (s *simulator) place(ctx context.Context, res *result, req *apiScriptlet.InstancePlacement, candidates []*api.ClusterMember, timeout time.Duration) (string, error) {
	res.Candidates = memberNames(candidates)

	// The daemon orders candidates by their instance count across all projects.
	res.InstanceCounts = map[string]int{}
	for _, name := range res.Candidates {
		res.InstanceCounts[name] = s.instancesCount("", name)
	}

	if !s.hasScriptlet {
		return "", nil
	}

	if timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	start := time.Now()
	target, err := placement.Run(ctx, &recorder{res: res}, req, candidates, &simProvider{sim: s, res: res})
	elapsed := time.Since(start)
	res.Duration = elapsed.Round(time.Microsecond).String()
	res.DurationMicros = elapsed.Microseconds()

	return target, err
}

// reject marks a placement as failed.
func (res *result) reject(err error) {
	res.Outcome = outcomeRejected
	res.Target = ""
	res.Error = err.Error()
}

// fallbackDetail explains why the built-in placement logic was used.
func (s *simulator) fallbackDetail() string {
	if !s.hasScriptlet {
		return "no scriptlet loaded, candidate with the fewest instances used"
	}

	return "scriptlet didn't call set_target, candidate with the fewest instances used"
}

// createOptions describes a new instance request.
type createOptions struct {
	Project      string
	Name         string
	Type         string
	Profiles     []string
	Config       map[string]string
	Devices      map[string]map[string]string
	Target       string
	Architecture string
	Image        string
}

// create simulates the creation of a new instance and records it as an action.
func (s *simulator) create(ctx context.Context, opts createOptions) (*result, error) {
	res, err := s.createInstance(ctx, opts)

	params := map[string]any{"project": opts.Project, "name": opts.Name, "type": opts.Type}
	optional := map[string]string{"target": opts.Target, "architecture": opts.Architecture, "image": opts.Image}
	for name, value := range optional {
		if value != "" {
			params[name] = value
		}
	}

	if opts.Profiles != nil {
		params["profiles"] = opts.Profiles
	}

	if len(opts.Config) > 0 {
		params["config"] = opts.Config
	}

	if len(opts.Devices) > 0 {
		params["devices"] = opts.Devices
	}

	s.record("create", params, err, res)

	return res, err
}

// createInstance mirrors instancesPost in the daemon.
func (s *simulator) createInstance(ctx context.Context, opts createOptions) (*result, error) {
	res := &result{Reason: apiScriptlet.InstancePlacementReasonNew, Project: opts.Project, Instance: opts.Name}

	p, err := s.project(opts.Project)
	if err != nil {
		return nil, err
	}

	if s.instance(opts.Project, opts.Name) != nil {
		return nil, fmt.Errorf("Instance %q already exists", opts.Name)
	}

	targetMember, targetGroup, err := s.checkTarget(p, opts.Target)
	if err != nil {
		return nil, err
	}

	profileNames := opts.Profiles
	if profileNames == nil {
		profileNames = []string{"default"}
	}

	profiles, err := s.profiles(p, profileNames)
	if err != nil {
		return nil, err
	}

	// Device overrides start from the profile device of the same name (as "incus init --device" does).
	profileDevices := expandDevices(nil, profiles)
	devices := map[string]map[string]string{}
	for deviceName, override := range opts.Devices {
		device := profileDevices[deviceName]
		if device == nil {
			device = map[string]string{}
		}

		maps.Copy(device, override)
		devices[deviceName] = device
	}

	// Build the request the way the client would send it.
	req := apiScriptlet.InstancePlacement{
		InstancesPost: api.InstancesPost{
			Name: opts.Name,
			Type: api.InstanceType(opts.Type),
			InstancePut: api.InstancePut{
				Architecture: opts.Architecture,
				Config:       maps.Clone(opts.Config),
				Devices:      devices,
				Profiles:     profileNames,
			},
			Source: api.InstanceSource{Type: "none"},
		},
		Project: opts.Project,
		Reason:  apiScriptlet.InstancePlacementReasonNew,
	}

	if req.Config == nil {
		req.Config = map[string]string{}
	}

	if opts.Image != "" {
		req.Source = api.InstanceSource{Type: "image", Alias: opts.Image}
	}

	localConfig := maps.Clone(map[string]string(req.Config))
	localDevices := expandDevices(req.Devices, nil)

	var candidates []*api.ClusterMember
	if targetMember != nil {
		// If a target was specified, limit the list of candidates to that target.
		candidates = []*api.ClusterMember{&targetMember.ClusterMember}
	} else {
		var architectures []int
		if opts.Architecture != "" {
			arch, err := osarch.ArchitectureID(opts.Architecture)
			if err != nil {
				return nil, err
			}

			architectures = []int{arch}
		}

		// Use the default architecture when the source doesn't dictate one.
		defaultArch := p.Config["images.default_architecture"]
		if defaultArch == "" {
			defaultArch = s.snap.Server.Config["images.default_architecture"]
		}

		if defaultArch != "" {
			defaultArchID, err := osarch.ArchitectureID(defaultArch)
			if err != nil {
				return nil, err
			}

			if len(architectures) < 1 || slices.Contains(architectures, defaultArchID) {
				architectures = []int{defaultArchID}
			}
		}

		candidates, res.Excluded, err = s.candidates(architectures, targetGroup, restrictedClusterGroups(p))
		if err != nil {
			return nil, err
		}
	}

	// The scriptlet gets the configuration and devices expanded with the profiles.
	req.Config = expandConfig(req.Config, profiles)
	req.Devices = expandDevices(req.Devices, profiles)
	res.describe(opts.Type, req.Config, req.Devices)

	target, err := s.place(ctx, res, &req, candidates, 0)
	if err != nil {
		res.reject(fmt.Errorf("Failed instance placement scriptlet: %w", err))
		return res, nil
	}

	switch {
	case target != "":
		res.Outcome = outcomeScriptlet
		res.Target = target
	case len(candidates) > 0:
		res.Outcome = outcomeBuiltin
		res.Target = candidates[0].ServerName
		res.Detail = s.fallbackDetail()
	default:
		res.reject(noCandidateError(p))
		return res, nil
	}

	// Record the new instance so that later placements can see it.
	if targetGroup != "" {
		localConfig["volatile.cluster.group"] = targetGroup
		req.Config["volatile.cluster.group"] = targetGroup
	}

	architecture := opts.Architecture
	if architecture == "" {
		architecture = s.member(res.Target).Architecture
	}

	instType := opts.Type
	if instType == "" {
		instType = string(api.InstanceTypeContainer)
	}

	s.snap.Instances = append(s.snap.Instances, api.Instance{
		InstancePut: api.InstancePut{
			Architecture: architecture,
			Config:       localConfig,
			Devices:      localDevices,
			Profiles:     profileNames,
		},
		ExpandedConfig:  req.Config,
		ExpandedDevices: req.Devices,
		Name:            opts.Name,
		Status:          "Stopped",
		StatusCode:      api.Stopped,
		Location:        res.Target,
		Type:            instType,
		Project:         opts.Project,
	})

	return res, nil
}

// move simulates moving an instance to another member or group and records it as an action.
func (s *simulator) move(ctx context.Context, projectName string, name string, target string) (*result, error) {
	res, err := s.moveInstance(ctx, projectName, name, target)
	s.record("move", map[string]any{"project": projectName, "instance": name, "target": target}, err, res)

	return res, err
}

// moveInstance mirrors instancePost in the daemon.
func (s *simulator) moveInstance(ctx context.Context, projectName string, name string, target string) (*result, error) {
	res := &result{Reason: apiScriptlet.InstancePlacementReasonRelocation, Project: projectName, Instance: name}

	inst := s.instance(projectName, name)
	if inst == nil {
		return nil, fmt.Errorf("Instance %q not found in project %q", name, projectName)
	}

	if target == "" {
		return nil, errors.New("A target is required as the daemon only considers a new location when one is requested")
	}

	res.Source = inst.Location
	res.describe(inst.Type, inst.ExpandedConfig, inst.ExpandedDevices)

	p, err := s.project(projectName)
	if err != nil {
		return nil, err
	}

	targetMember, targetGroup, err := s.checkTarget(p, target)
	if err != nil {
		return nil, err
	}

	var candidates []*api.ClusterMember
	if targetMember != nil {
		// If a target was specified, limit the list of candidates to that target.
		candidates = []*api.ClusterMember{&targetMember.ClusterMember}
	} else {
		arch, err := osarch.ArchitectureID(inst.Architecture)
		if err != nil {
			return nil, err
		}

		candidates, res.Excluded, err = s.candidates([]int{arch}, targetGroup, restrictedClusterGroups(p))
		if err != nil {
			return nil, err
		}
	}

	profiles, err := s.profiles(p, inst.Profiles)
	if err != nil {
		return nil, err
	}

	// The daemon doesn't pass the instance type or architecture for relocations.
	req := apiScriptlet.InstancePlacement{
		InstancesPost: api.InstancesPost{
			Name: name,
			Type: api.InstanceTypeAny,
			InstancePut: api.InstancePut{
				Config:   expandConfig(inst.Config, profiles),
				Devices:  expandDevices(inst.Devices, profiles),
				Profiles: inst.Profiles,
			},
		},
		Project: projectName,
		Reason:  apiScriptlet.InstancePlacementReasonRelocation,
	}

	scriptTarget, err := s.place(ctx, res, &req, candidates, 0)
	if err != nil {
		res.reject(fmt.Errorf("Failed instance placement scriptlet: %w", err))
		return res, nil
	}

	switch {
	case targetMember != nil:
		// The scriptlet can only veto an explicitly requested member.
		res.Outcome = outcomeValidated
		res.Target = targetMember.ServerName
		res.Detail = "explicit target, the scriptlet can only reject it"
	case scriptTarget != "":
		res.Outcome = outcomeScriptlet
		res.Target = scriptTarget
	default:
		// Pick the member with the fewest instances other than the current one.
		for _, candidate := range candidates {
			if candidate.ServerName != inst.Location {
				res.Outcome = outcomeBuiltin
				res.Target = candidate.ServerName
				res.Detail = s.fallbackDetail()
				break
			}
		}

		if res.Target == "" {
			res.reject(noCandidateError(p))
			return res, nil
		}
	}

	if s.member(res.Target).Status == "Offline" {
		res.reject(errors.New("Target cluster member is offline"))
		return res, nil
	}

	inst.Location = res.Target
	if targetGroup != "" {
		inst.Config["volatile.cluster.group"] = targetGroup
	}

	return res, nil
}

// evacuateAction mirrors evacuateInstanceAction and the instance CanMigrate logic in the daemon.
// The snapshot can't tell whether a device prevents migration, so devices are assumed to be migratable.
func evacuateAction(inst *api.Instance, heal bool) string {
	action := inst.ExpandedConfig["cluster.evacuate"]
	if action == "" || action == "auto" {
		action = "migrate"
		if inst.Type == string(api.InstanceTypeVM) && util.IsTrue(inst.ExpandedConfig["migration.stateful"]) {
			action = "live-migrate"
		}
	}

	if !heal {
		return action
	}

	// Source server is dead, live-migration isn't an option.
	if action == "live-migrate" || action == "refresh-migrate" {
		return "migrate"
	}

	// We can only migrate instances or leave them as they are.
	if action != "migrate" {
		return ""
	}

	return action
}

// evacuate simulates the evacuation or healing of a cluster member and records it as an action.
func (s *simulator) evacuate(ctx context.Context, memberName string, heal bool) ([]*result, error) {
	results, err := s.evacuateMember(ctx, memberName, heal)
	s.record("evacuate", map[string]any{"member": memberName, "heal": heal}, err, results...)

	return results, err
}

// evacuateMember mirrors evacuateInstances in the daemon.
// Instances are handled one at a time in snapshot order, with each move applied before the next placement.
func (s *simulator) evacuateMember(ctx context.Context, memberName string, heal bool) ([]*result, error) {
	member := s.member(memberName)
	if member == nil {
		return nil, fmt.Errorf("Cluster member %q not found", memberName)
	}

	if !isSchedulable(member) && !heal {
		return nil, fmt.Errorf("Cluster member %q is %s and can't be evacuated", memberName, strings.ToLower(member.Status))
	}

	if heal && s.localMember == memberName {
		return nil, fmt.Errorf("Cluster member %q is being healed and can't be the local member", memberName)
	}

	// Evacuation runs on the evacuated member while healing runs on the leader.
	if heal {
		member.Status = "Offline"
	} else {
		member.Status = "Evacuated"
		s.localMember = memberName
	}

	results := []*result{}
	for i := range s.snap.Instances {
		inst := &s.snap.Instances[i]
		if inst.Location != memberName {
			continue
		}

		res, err := s.evacuateInstance(ctx, inst, memberName, heal)
		if err != nil {
			return results, err
		}

		// Later placements change the cluster further, so its state is captured as it is now.
		res.MembersAfter = s.memberStates()
		results = append(results, res)
	}

	return results, nil
}

// evacuateInstance places a single instance of a cluster member being evacuated or healed.
func (s *simulator) evacuateInstance(ctx context.Context, inst *api.Instance, memberName string, heal bool) (*result, error) {
	res := &result{Reason: apiScriptlet.InstancePlacementReasonEvacuation, Project: inst.Project, Instance: inst.Name, Source: memberName}
	res.describe(inst.Type, inst.ExpandedConfig, inst.ExpandedDevices)

	action := evacuateAction(inst, heal)
	if action != "migrate" && action != "live-migrate" && action != "refresh-migrate" {
		res.Outcome = outcomeStays
		res.Target = memberName
		res.Detail = "instance isn't migrated (cluster.evacuate=" + inst.ExpandedConfig["cluster.evacuate"] + ")"
		if action == "" {
			res.Detail = "instance is left as is during healing (cluster.evacuate=" + inst.ExpandedConfig["cluster.evacuate"] + ")"
		}

		return res, nil
	}

	p, err := s.project(inst.Project)
	if err != nil {
		return nil, err
	}

	arch, err := osarch.ArchitectureID(inst.Architecture)
	if err != nil {
		return nil, err
	}

	// Filter candidates by the instance's cluster group and offline servers.
	candidates, excluded, err := s.candidates([]int{arch}, inst.Config["volatile.cluster.group"], restrictedClusterGroups(p))
	if err != nil {
		return nil, err
	}

	res.Excluded = excluded

	// Nothing for the scriptlet to choose from, so the instance is stopped in place.
	if len(candidates) == 0 {
		res.Candidates = []string{}
		res.Outcome = outcomeStays
		res.Target = memberName
		res.Detail = "no migration target available, instance is stopped in place"

		return res, nil
	}

	req := apiScriptlet.InstancePlacement{
		InstancesPost: api.InstancesPost{
			Name: inst.Name,
			Type: api.InstanceType(inst.Type),
			InstancePut: api.InstancePut{
				Architecture: inst.Architecture,
				Config:       maps.Clone(inst.ExpandedConfig),
				Devices:      expandDevices(inst.ExpandedDevices, nil),
				Profiles:     inst.Profiles,
			},
		},
		Project: inst.Project,
		Reason:  apiScriptlet.InstancePlacementReasonEvacuation,
	}

	target, err := s.place(ctx, res, &req, candidates, evacuationTimeout)
	if err != nil {
		res.reject(fmt.Errorf("Failed instance placement scriptlet for instance %q in project %q: %w", inst.Name, inst.Project, err))
		return res, nil
	}

	if target != "" {
		res.Outcome = outcomeScriptlet
		res.Target = target
	} else {
		res.Outcome = outcomeBuiltin
		res.Target = candidates[0].ServerName
		res.Detail = s.fallbackDetail()
	}

	inst.Location = res.Target

	return res, nil
}
