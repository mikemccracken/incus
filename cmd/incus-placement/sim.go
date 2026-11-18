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

// traceEntry is a query made or a message logged by the scriptlet, in the order they happened.
type traceEntry struct {
	Kind    string `json:"kind" yaml:"kind"`
	Message string `json:"message" yaml:"message"`
}

// exclusion records why a cluster member wasn't a placement candidate.
type exclusion struct {
	Member string `json:"member" yaml:"member"`
	Reason string `json:"reason" yaml:"reason"`
}

// result describes what happened to a single instance placement.
type result struct {
	Reason         string         `json:"reason" yaml:"reason"`
	Project        string         `json:"project" yaml:"project"`
	Instance       string         `json:"instance" yaml:"instance"`
	Source         string         `json:"source,omitempty" yaml:"source,omitempty"`
	Candidates     []string       `json:"candidates" yaml:"candidates"`
	InstanceCounts map[string]int `json:"instance_counts,omitempty" yaml:"instance_counts,omitempty"`
	Excluded       []exclusion    `json:"excluded,omitempty" yaml:"excluded,omitempty"`
	Outcome        string         `json:"outcome" yaml:"outcome"`
	Target         string         `json:"target,omitempty" yaml:"target,omitempty"`
	Detail         string         `json:"detail,omitempty" yaml:"detail,omitempty"`
	Error          string         `json:"error,omitempty" yaml:"error,omitempty"`
	Trace          []traceEntry   `json:"trace,omitempty" yaml:"trace,omitempty"`
	Duration       string         `json:"duration,omitempty" yaml:"duration,omitempty"`
}

// recorder is a logger which records the scriptlet messages in a result.
type recorder struct {
	res *result
}

func (r *recorder) record(level string, msg string, args []logger.Ctx) {
	msg = strings.TrimPrefix(msg, "Instance placement scriptlet: ")

	for _, ctx := range args {
		keys := slices.Sorted(maps.Keys(ctx))
		for _, key := range keys {
			msg = fmt.Sprintf("%s %s=%v", msg, key, ctx[key])
		}
	}

	r.res.Trace = append(r.res.Trace, traceEntry{Kind: level, Message: msg})
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

	return s, nil
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

// defaultProject picks the project to use when none was requested.
func (s *simulator) defaultProject(name string) string {
	if name != "" {
		return name
	}

	if len(s.snap.Projects) == 1 {
		return s.snap.Projects[0].Name
	}

	return api.ProjectDefaultName
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
	return util.SplitNTrimSpace(p.Config["restricted.cluster.groups"], ",", -1, true)
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
func (s *simulator) exclusionReason(member *SnapshotMember, architectures []int, group string, allowedGroups []string) (string, error) {
	// Skip pending, evacuated or offline members.
	if !isSchedulable(member) {
		return "status is " + member.Status, nil
	}

	// Skip manually targeted members.
	if member.Config["scheduler.instance"] == "manual" {
		return "scheduler.instance=manual", nil
	}

	// Skip group-only members if targeted cluster group doesn't match.
	if member.Config["scheduler.instance"] == "group" && !slices.Contains(member.Groups, group) {
		if group == "" {
			return "scheduler.instance=group and no cluster group was targeted", nil
		}

		return fmt.Sprintf("scheduler.instance=group and not in cluster group %q", group), nil
	}

	// Skip if a group is requested and member isn't part of it.
	if group != "" && !slices.Contains(member.Groups, group) {
		return fmt.Sprintf("not in cluster group %q", group), nil
	}

	// Skip if working with a restricted set of cluster groups and member isn't part of any.
	if allowedGroups != nil {
		found := false
		for _, allowedGroup := range allowedGroups {
			if !s.groupExists(allowedGroup) {
				return "", fmt.Errorf("Cluster group %q doesn't exist", allowedGroup)
			}

			if slices.Contains(member.Groups, allowedGroup) {
				found = true
				break
			}
		}

		if !found {
			return "not in a cluster group allowed by the project (" + strings.Join(allowedGroups, ", ") + ")", nil
		}
	}

	// Consider target architectures if specified.
	if architectures != nil {
		memberArch, err := osarch.ArchitectureID(member.Architecture)
		if err != nil {
			return "", err
		}

		personalities, err := osarch.ArchitecturePersonalities(memberArch)
		if err != nil {
			return "", err
		}

		supported := append([]int{memberArch}, personalities...)
		for _, arch := range supported {
			if slices.Contains(architectures, arch) {
				return "", nil
			}
		}

		return fmt.Sprintf("architecture is %s, instance needs %s", member.Architecture, architectureNames(architectures)), nil
	}

	return "", nil
}

// candidates mirrors db.GetCandidateMembers and also reports why the other members were left out.
func (s *simulator) candidates(architectures []int, group string, allowedGroups []string) ([]*api.ClusterMember, []exclusion, error) {
	var candidateMembers []*api.ClusterMember
	var excluded []exclusion

	for i := range s.snap.Members {
		member := &s.snap.Members[i]

		reason, err := s.exclusionReason(member, architectures, group, allowedGroups)
		if err != nil {
			return nil, nil, err
		}

		if reason != "" {
			excluded = append(excluded, exclusion{Member: member.ServerName, Reason: reason})
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

	if !isGroup && target != "" {
		member := s.member(target)
		if member == nil {
			return nil, "", fmt.Errorf("Cluster member %q not found", target)
		}

		if util.IsTrue(p.Config["restricted"]) && len(allowedGroups) > 0 {
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
		if !util.IsFalseOrEmpty(p.Config["restricted"]) && len(allowedGroups) > 0 && !slices.Contains(allowedGroups, groupName) {
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

func (p *simProvider) call(format string, args ...any) {
	p.res.Trace = append(p.res.Trace, traceEntry{Kind: "call", Message: fmt.Sprintf(format, args...)})
}

// LocalMember returns the member simulated as running the scriptlet.
func (p *simProvider) LocalMember() string {
	return p.sim.localMember
}

// ClusterMemberResources returns the captured resources of a cluster member.
func (p *simProvider) ClusterMemberResources(_ context.Context, memberName string) (*api.Resources, error) {
	p.call("get_cluster_member_resources(%q)", memberName)

	member := p.sim.member(memberName)
	if member == nil || member.Resources == nil {
		return nil, fmt.Errorf("No resources captured for cluster member %q", memberName)
	}

	return member.Resources, nil
}

// ClusterMemberState returns the captured state of a cluster member.
func (p *simProvider) ClusterMemberState(_ context.Context, memberName string) (*api.ClusterMemberState, error) {
	p.call("get_cluster_member_state(%q)", memberName)

	member := p.sim.member(memberName)
	if member == nil || member.State == nil {
		return nil, fmt.Errorf("No state captured for cluster member %q", memberName)
	}

	return member.State, nil
}

// InstanceResources returns the resources required by the instance being placed.
func (p *simProvider) InstanceResources(req *apiScriptlet.InstancePlacement) (*apiScriptlet.InstanceResources, error) {
	p.call("get_instance_resources()")

	return instanceResources(req.Config, req.Devices, req.Type)
}

// Instances returns the instances matching the optional project and location filters.
func (p *simProvider) Instances(_ context.Context, projectName string, location string) ([]api.Instance, error) {
	p.call("get_instances(project=%q, location=%q)", projectName, location)

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
	p.call("get_instances_count(project=%q, location=%q, pending=%t)", projectName, location, includePending)

	return p.sim.instancesCount(projectName, location), nil
}

// ClusterMembers returns the cluster members available for placement, optionally limited to a group.
func (p *simProvider) ClusterMembers(_ context.Context, group string) ([]*api.ClusterMember, error) {
	p.call("get_cluster_members(group=%q)", group)

	members, _, err := p.sim.candidates(nil, group, nil)

	return members, err
}

// Project returns the named project.
func (p *simProvider) Project(_ context.Context, name string) (*api.Project, error) {
	p.call("get_project(%q)", name)

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
	res.Duration = time.Since(start).Round(time.Microsecond).String()

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

// create simulates the creation of a new instance (mirrors instancesPost in the daemon).
func (s *simulator) create(ctx context.Context, opts createOptions) (*result, error) {
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
		res.reject(errors.New("Couldn't find a cluster member for the instance"))
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

// move simulates moving an instance to another member or group (mirrors instancePost in the daemon).
func (s *simulator) move(ctx context.Context, projectName string, name string, target string) (*result, error) {
	res := &result{Reason: apiScriptlet.InstancePlacementReasonRelocation, Project: projectName, Instance: name}

	inst := s.instance(projectName, name)
	if inst == nil {
		return nil, fmt.Errorf("Instance %q not found in project %q", name, projectName)
	}

	if target == "" {
		return nil, errors.New("A target is required as the daemon only considers a new location when one is requested")
	}

	res.Source = inst.Location

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
			res.reject(errors.New("Couldn't find a cluster member for the instance"))
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

// evacuate simulates the evacuation or healing of a cluster member (mirrors evacuateInstances in the daemon).
// Instances are handled one at a time in snapshot order, with each move applied before the next placement.
func (s *simulator) evacuate(ctx context.Context, memberName string, heal bool) ([]*result, error) {
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

		res := &result{Reason: apiScriptlet.InstancePlacementReasonEvacuation, Project: inst.Project, Instance: inst.Name, Source: memberName}
		results = append(results, res)

		action := evacuateAction(inst, heal)
		if action != "migrate" && action != "live-migrate" && action != "refresh-migrate" {
			res.Outcome = outcomeStays
			res.Target = memberName
			res.Detail = "instance isn't migrated (cluster.evacuate=" + inst.ExpandedConfig["cluster.evacuate"] + ")"
			if action == "" {
				res.Detail = "instance is left as is during healing (cluster.evacuate=" + inst.ExpandedConfig["cluster.evacuate"] + ")"
			}

			continue
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

			continue
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
			continue
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
	}

	return results, nil
}
