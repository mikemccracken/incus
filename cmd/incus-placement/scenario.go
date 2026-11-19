package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/lxc/incus/v7/shared/api"
)

// scenario is a sequence of actions along with what is expected from each of them.
type scenario struct {
	Description string         `yaml:"description"`
	Cluster     string         `yaml:"cluster"`
	Scriptlet   string         `yaml:"scriptlet"`
	LocalMember string         `yaml:"local_member"`
	Steps       []scenarioStep `yaml:"steps"`
}

// scenarioStep is a single action, exactly one of create, move or evacuate.
type scenarioStep struct {
	Name     string            `yaml:"name"`
	Create   *scenarioCreate   `yaml:"create"`
	Move     *scenarioMove     `yaml:"move"`
	Evacuate *scenarioEvacuate `yaml:"evacuate"`
	Expect   scenarioExpect    `yaml:"expect"`
}

// scenarioCreate describes the creation of an instance.
type scenarioCreate struct {
	Project      string                       `yaml:"project"`
	Name         string                       `yaml:"name"`
	Type         string                       `yaml:"type"`
	Profiles     []string                     `yaml:"profiles"`
	Config       map[string]string            `yaml:"config"`
	Devices      map[string]map[string]string `yaml:"devices"`
	Target       string                       `yaml:"target"`
	Architecture string                       `yaml:"architecture"`
	Image        string                       `yaml:"image"`
}

// scenarioMove describes moving an instance to a cluster member or group.
type scenarioMove struct {
	Project  string `yaml:"project"`
	Instance string `yaml:"instance"`
	Target   string `yaml:"target"`
}

// scenarioEvacuate describes the evacuation (or healing) of a cluster member.
type scenarioEvacuate struct {
	Member string `yaml:"member"`
	Heal   bool   `yaml:"heal"`
}

// placementExpect is what is expected from the placement of an instance.
type placementExpect struct {
	Outcome   string   `yaml:"outcome"`
	Target    string   `yaml:"target"`
	TargetIn  []string `yaml:"target_in"`
	TargetNot []string `yaml:"target_not"`
	Error     string   `yaml:"error"`
}

// memberExpect is the number of instances expected on a cluster member.
type memberExpect struct {
	Instances    *int `yaml:"instances"`
	MinInstances *int `yaml:"min_instances"`
	MaxInstances *int `yaml:"max_instances"`
}

// scenarioExpect is what is expected once a step has run.
type scenarioExpect struct {
	placementExpect `yaml:",inline"`

	Instances map[string]placementExpect `yaml:"instances"`
	Members   map[string]memberExpect    `yaml:"members"`
	Apart     []string                   `yaml:"apart"`
}

// stepReport is the outcome of a scenario step.
type stepReport struct {
	Name     string
	Results  []*result
	Failures []string
}

// loadScenario reads and validates a scenario file.
func loadScenario(path string) (*scenario, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	sc := &scenario{}

	// Unknown fields are refused so that a mistyped expectation can't silently pass.
	err = yaml.Load(content, sc, yaml.WithKnownFields())
	if err != nil {
		return nil, fmt.Errorf("Failed parsing scenario %q: %w", path, err)
	}

	if len(sc.Steps) == 0 {
		return nil, fmt.Errorf("Scenario %q doesn't have any step", path)
	}

	for i := range sc.Steps {
		err = sc.Steps[i].validate()
		if err != nil {
			return nil, fmt.Errorf("Invalid step %d in scenario %q: %w", i+1, path, err)
		}
	}

	return sc, nil
}

// validate checks that a step is usable.
func (step *scenarioStep) validate() error {
	actions := 0
	if step.Create != nil {
		actions++

		if step.Create.Name == "" {
			return errors.New("An instance name is required")
		}
	}

	if step.Move != nil {
		actions++

		if step.Move.Instance == "" || step.Move.Target == "" {
			return errors.New("An instance and a target are required")
		}
	}

	if step.Evacuate != nil {
		actions++

		if step.Evacuate.Member == "" {
			return errors.New("A cluster member is required")
		}
	}

	if actions != 1 {
		return errors.New("Exactly one of create, move or evacuate is required")
	}

	expects := []placementExpect{step.Expect.placementExpect}
	for _, expect := range step.Expect.Instances {
		expects = append(expects, expect)
	}

	outcomes := []string{"", outcomeScriptlet, outcomeBuiltin, outcomeValidated, outcomeRejected, outcomeStays}
	for _, expect := range expects {
		if !slices.Contains(outcomes, expect.Outcome) {
			return fmt.Errorf("Invalid outcome %q", expect.Outcome)
		}
	}

	return nil
}

// label returns the name of a step, describing its action if it wasn't given one.
func (step *scenarioStep) label() string {
	if step.Name != "" {
		return step.Name
	}

	switch {
	case step.Create != nil:
		return "create " + step.Create.Name
	case step.Move != nil:
		return fmt.Sprintf("move %s to %s", step.Move.Instance, step.Move.Target)
	case step.Evacuate.Heal:
		return "heal " + step.Evacuate.Member
	}

	return "evacuate " + step.Evacuate.Member
}

// execute runs the action of a step.
func (step *scenarioStep) execute(ctx context.Context, sim *simulator) ([]*result, error) {
	if step.Evacuate != nil {
		return sim.evacuate(ctx, step.Evacuate.Member, step.Evacuate.Heal)
	}

	if step.Move != nil {
		res, err := sim.move(ctx, sim.defaultProject(step.Move.Project), step.Move.Instance, step.Move.Target)
		if err != nil {
			return nil, err
		}

		return []*result{res}, nil
	}

	opts := createOptions{
		Project:      sim.defaultProject(step.Create.Project),
		Name:         step.Create.Name,
		Type:         step.Create.Type,
		Profiles:     step.Create.Profiles,
		Config:       step.Create.Config,
		Devices:      step.Create.Devices,
		Target:       step.Create.Target,
		Architecture: step.Create.Architecture,
		Image:        step.Create.Image,
	}

	switch opts.Type {
	case "":
		opts.Type = string(api.InstanceTypeContainer)
	case "vm":
		opts.Type = string(api.InstanceTypeVM)
	case string(api.InstanceTypeContainer), string(api.InstanceTypeVM):
	default:
		return nil, fmt.Errorf("Invalid instance type %q", opts.Type)
	}

	if opts.Config == nil {
		opts.Config = map[string]string{}
	}

	if opts.Devices == nil {
		opts.Devices = map[string]map[string]string{}
	}

	res, err := sim.create(ctx, opts)
	if err != nil {
		return nil, err
	}

	return []*result{res}, nil
}

// describe summarizes what happened to an instance.
func (res *result) describe() string {
	switch res.Outcome {
	case outcomeRejected:
		return "a rejection (" + res.Error + ")"
	case outcomeStays:
		return "it staying on " + res.Target
	}

	return fmt.Sprintf("%s (%s)", res.Target, res.Outcome)
}

// check returns how a placement differs from what was expected.
func (e placementExpect) check(res *result) []string {
	var failures []string

	fail := func(format string, args ...any) {
		failures = append(failures, fmt.Sprintf("%s/%s: expected ", res.Project, res.Instance)+fmt.Sprintf(format, args...)+", got "+res.describe())
	}

	if e.Outcome != "" && res.Outcome != e.Outcome {
		fail("outcome %q", e.Outcome)
	}

	if e.Target != "" && res.Target != e.Target {
		fail("target %s", e.Target)
	}

	if len(e.TargetIn) > 0 && !slices.Contains(e.TargetIn, res.Target) {
		fail("a target among %s", strings.Join(e.TargetIn, ", "))
	}

	if slices.Contains(e.TargetNot, res.Target) {
		fail("a target other than %s", strings.Join(e.TargetNot, ", "))
	}

	if e.Error != "" && !strings.Contains(res.Error, e.Error) {
		fail("an error containing %q", e.Error)
	}

	return failures
}

// matches returns whether an instance reference ("name" or "project/name") designates the given instance.
func instanceMatches(ref string, projectName string, name string) bool {
	refProject, refName, found := strings.Cut(ref, "/")
	if !found {
		return ref == name
	}

	return refProject == projectName && refName == name
}

// check returns how the outcome of a step differs from what was expected.
func (e *scenarioExpect) check(sim *simulator, results []*result) []string {
	var failures []string

	// Placements, with the per-instance expectations taking over the step wide ones.
	seen := map[string]bool{}
	for _, res := range results {
		expect := e.placementExpect
		for ref, instanceExpect := range e.Instances {
			if instanceMatches(ref, res.Project, res.Instance) {
				expect = instanceExpect
				seen[ref] = true
			}
		}

		failures = append(failures, expect.check(res)...)
	}

	for ref := range e.Instances {
		if !seen[ref] {
			failures = append(failures, fmt.Sprintf("%s: expected a placement, but the step didn't place this instance", ref))
		}
	}

	// Instance counts, with "*" covering the members which aren't listed.
	for _, member := range sim.snap.Members {
		expect, ok := e.Members[member.ServerName]
		if !ok {
			expect, ok = e.Members["*"]
			if !ok {
				continue
			}
		}

		count := sim.instancesCount("", member.ServerName)
		got := ", got " + instancesLabel(count)

		if expect.Instances != nil && count != *expect.Instances {
			failures = append(failures, fmt.Sprintf("member %s: expected %s%s", member.ServerName, instancesLabel(*expect.Instances), got))
		}

		if expect.MinInstances != nil && count < *expect.MinInstances {
			failures = append(failures, fmt.Sprintf("member %s: expected at least %s%s", member.ServerName, instancesLabel(*expect.MinInstances), got))
		}

		if expect.MaxInstances != nil && count > *expect.MaxInstances {
			failures = append(failures, fmt.Sprintf("member %s: expected at most %s%s", member.ServerName, instancesLabel(*expect.MaxInstances), got))
		}
	}

	for name := range e.Members {
		if name != "*" && sim.member(name) == nil {
			failures = append(failures, fmt.Sprintf("member %s: not found in the cluster", name))
		}
	}

	// Instances which must all be on different members.
	locations := map[string][]string{}
	for _, ref := range e.Apart {
		found := false
		for _, inst := range sim.snap.Instances {
			if instanceMatches(ref, inst.Project, inst.Name) {
				locations[inst.Location] = append(locations[inst.Location], ref)
				found = true
			}
		}

		if !found {
			failures = append(failures, fmt.Sprintf("%s: expected to be apart from others, but the instance doesn't exist", ref))
		}
	}

	shared := make([]string, 0, len(locations))
	for location, refs := range locations {
		if len(refs) > 1 {
			shared = append(shared, fmt.Sprintf("expected %s to be apart, but they are all on %s", strings.Join(refs, ", "), location))
		}
	}

	sort.Strings(shared)

	return append(failures, shared...)
}

// run plays all the steps against the simulator, which carries its state from one step to the next.
func (sc *scenario) run(ctx context.Context, sim *simulator) []stepReport {
	localMember := sim.localMember

	reports := make([]stepReport, 0, len(sc.Steps))
	for i := range sc.Steps {
		step := &sc.Steps[i]
		rep := stepReport{Name: step.label()}

		results, err := step.execute(ctx, sim)

		// An evacuation is handled by the evacuated member, later steps go back to the usual one.
		sim.localMember = localMember

		// A step which can't run fails on its own so that the other steps still get checked.
		if err != nil {
			rep.Failures = []string{"couldn't run: " + err.Error()}
		} else {
			rep.Results = results
			rep.Failures = step.Expect.check(sim, results)
		}

		reports = append(reports, rep)
	}

	return reports
}
