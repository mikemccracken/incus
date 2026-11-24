package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.starlark.net/starlark"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/scriptlet"
)

// testRunKey is the thread local holding the state of the test being run.
const testRunKey = "test"

// scenario is a Starlark file holding test functions along with the cluster and scriptlet they apply to.
type scenario struct {
	Path        string
	Cluster     string
	Scriptlet   string
	LocalMember string
	Tests       []*starlark.Function
}

// testRun is the state of a test function while it runs.
type testRun struct {
	ctx     context.Context
	sim     *simulator
	results []*result
	output  []string
}

// testReport is the outcome of a test function.
type testReport struct {
	Name    string
	Results []*result
	Output  []string
	Failure string
}

// currentTest returns the test a builtin was called from.
func currentTest(thread *starlark.Thread, b *starlark.Builtin) (*testRun, error) {
	run, ok := thread.Local(testRunKey).(*testRun)
	if !ok {
		return nil, fmt.Errorf("%s can only be called from a test function", b.Name())
	}

	return run, nil
}

// stringValues converts a Starlark dict into a string map, accepting numbers and booleans as values.
func stringValues(dict *starlark.Dict) (map[string]string, error) {
	result := map[string]string{}
	if dict == nil {
		return result, nil
	}

	for _, item := range dict.Items() {
		key, ok := starlark.AsString(item[0])
		if !ok {
			return nil, fmt.Errorf("Invalid key %s, expected a string", item[0].String())
		}

		switch value := item[1].(type) {
		case starlark.String:
			result[key] = string(value)
		case starlark.Int, starlark.Bool:
			result[key] = strings.ToLower(value.String())
		default:
			return nil, fmt.Errorf("Invalid value for %q, expected a string, integer or boolean", key)
		}
	}

	return result, nil
}

// record keeps a placement for the test report and hands it to the scenario.
func (run *testRun) record(res *result) (starlark.Value, error) {
	run.results = append(run.results, res)

	return scriptlet.StarlarkMarshal(*res)
}

// scenarioCreate implements create().
func scenarioCreate(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	run, err := currentTest(thread, b)
	if err != nil {
		return nil, err
	}

	var profiles starlark.Value
	var config *starlark.Dict
	var devices *starlark.Dict

	opts := createOptions{Type: string(api.InstanceTypeContainer), Devices: map[string]map[string]string{}}

	err = starlark.UnpackArgs(b.Name(), args, kwargs, "name", &opts.Name, "project?", &opts.Project, "type?", &opts.Type, "profiles?", &profiles, "config?", &config, "devices?", &devices, "target?", &opts.Target, "architecture?", &opts.Architecture, "image?", &opts.Image)
	if err != nil {
		return nil, err
	}

	if opts.Type == "vm" {
		opts.Type = string(api.InstanceTypeVM)
	}

	if opts.Type != string(api.InstanceTypeContainer) && opts.Type != string(api.InstanceTypeVM) {
		return nil, fmt.Errorf("Invalid instance type %q", opts.Type)
	}

	// The default profile is used unless a list is given, which may be empty.
	if profiles != nil && profiles != starlark.None {
		list, ok := profiles.(*starlark.List)
		if !ok {
			return nil, errors.New("Invalid profiles, expected a list")
		}

		opts.Profiles = []string{}
		for i := range list.Len() {
			name, ok := starlark.AsString(list.Index(i))
			if !ok {
				return nil, errors.New("Invalid profiles, expected a list of names")
			}

			opts.Profiles = append(opts.Profiles, name)
		}
	}

	opts.Config, err = stringValues(config)
	if err != nil {
		return nil, err
	}

	if devices != nil {
		for _, item := range devices.Items() {
			name, isName := starlark.AsString(item[0])
			deviceConfig, isDict := item[1].(*starlark.Dict)
			if !isName || !isDict {
				return nil, errors.New("Invalid devices, expected a dict of device name to configuration")
			}

			opts.Devices[name], err = stringValues(deviceConfig)
			if err != nil {
				return nil, err
			}
		}
	}

	opts.Project = run.sim.defaultProject(opts.Project)

	res, err := run.sim.create(run.ctx, opts)
	if err != nil {
		return nil, err
	}

	return run.record(res)
}

// scenarioMove implements move().
func scenarioMove(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	run, err := currentTest(thread, b)
	if err != nil {
		return nil, err
	}

	var name string
	var target string
	var projectName string

	err = starlark.UnpackArgs(b.Name(), args, kwargs, "instance", &name, "target", &target, "project?", &projectName)
	if err != nil {
		return nil, err
	}

	res, err := run.sim.move(run.ctx, run.sim.defaultProject(projectName), name, target)
	if err != nil {
		return nil, err
	}

	return run.record(res)
}

// scenarioEvacuate implements evacuate().
func scenarioEvacuate(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	run, err := currentTest(thread, b)
	if err != nil {
		return nil, err
	}

	var member string
	var heal bool

	err = starlark.UnpackArgs(b.Name(), args, kwargs, "member", &member, "heal?", &heal)
	if err != nil {
		return nil, err
	}

	// An evacuation is handled by the evacuated member, later actions go back to the usual one.
	localMember := run.sim.localMember
	results, err := run.sim.evacuate(run.ctx, member, heal)
	run.sim.localMember = localMember

	if err != nil {
		return nil, err
	}

	// Placements are keyed by instance name, prefixed with the project outside of the default one.
	defaultProject := run.sim.defaultProject("")

	placed := starlark.NewDict(len(results))
	for _, res := range results {
		value, err := run.record(res)
		if err != nil {
			return nil, err
		}

		key := res.Instance
		if res.Project != defaultProject {
			key = res.Project + "/" + res.Instance
		}

		err = placed.SetKey(starlark.String(key), value)
		if err != nil {
			return nil, err
		}
	}

	return placed, nil
}

// scenarioQuery wraps one of the read-only functions that placement scriptlets also have.
func scenarioQuery(query func(provider *simProvider, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (any, error)) func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		run, err := currentTest(thread, b)
		if err != nil {
			return nil, err
		}

		value, err := query(&simProvider{sim: run.sim, res: &result{}}, b, args, kwargs)
		if err != nil {
			return nil, err
		}

		return scriptlet.StarlarkMarshal(value)
	}
}

// scenarioAssertEq implements assert_eq().
func scenarioAssertEq(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var got starlark.Value
	var want starlark.Value
	var msg string

	err := starlark.UnpackArgs(b.Name(), args, kwargs, "got", &got, "want", &want, "msg?", &msg)
	if err != nil {
		return nil, err
	}

	equal, err := starlark.Equal(got, want)
	if err != nil {
		return nil, err
	}

	if !equal {
		if msg != "" {
			msg = ": " + msg
		}

		return nil, fmt.Errorf("got %s, want %s%s", got.String(), want.String(), msg)
	}

	return starlark.None, nil
}

// scenarioAssertTrue implements assert_true().
func scenarioAssertTrue(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var cond starlark.Value
	var msg string

	err := starlark.UnpackArgs(b.Name(), args, kwargs, "cond", &cond, "msg?", &msg)
	if err != nil {
		return nil, err
	}

	if !cond.Truth() {
		if msg == "" {
			msg = "condition is false"
		}

		return nil, errors.New(msg)
	}

	return starlark.None, nil
}

// scenarioBuiltins returns what a scenario can call on top of the Starlark language.
func scenarioBuiltins() starlark.StringDict {
	builtins := map[string]func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error){
		"create":      scenarioCreate,
		"move":        scenarioMove,
		"evacuate":    scenarioEvacuate,
		"assert_eq":   scenarioAssertEq,
		"assert_true": scenarioAssertTrue,
		"get_instances": scenarioQuery(func(provider *simProvider, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (any, error) {
			var projectName string
			var location string

			err := starlark.UnpackArgs(b.Name(), args, kwargs, "project??", &projectName, "location??", &location)
			if err != nil {
				return nil, err
			}

			return provider.Instances(context.Background(), projectName, location)
		}),
		"get_instances_count": scenarioQuery(func(provider *simProvider, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (any, error) {
			var projectName string
			var location string
			var pending bool

			err := starlark.UnpackArgs(b.Name(), args, kwargs, "project??", &projectName, "location??", &location, "pending??", &pending)
			if err != nil {
				return nil, err
			}

			return provider.InstancesCount(context.Background(), projectName, location, pending)
		}),
		"get_cluster_members": scenarioQuery(func(provider *simProvider, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (any, error) {
			var group string

			err := starlark.UnpackArgs(b.Name(), args, kwargs, "group??", &group)
			if err != nil {
				return nil, err
			}

			return provider.ClusterMembers(context.Background(), group)
		}),
		"get_cluster_member_resources": scenarioQuery(func(provider *simProvider, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (any, error) {
			var member string

			err := starlark.UnpackArgs(b.Name(), args, kwargs, "member_name", &member)
			if err != nil {
				return nil, err
			}

			return provider.ClusterMemberResources(context.Background(), member)
		}),
		"get_cluster_member_state": scenarioQuery(func(provider *simProvider, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (any, error) {
			var member string

			err := starlark.UnpackArgs(b.Name(), args, kwargs, "member_name", &member)
			if err != nil {
				return nil, err
			}

			return provider.ClusterMemberState(context.Background(), member)
		}),
		"get_project": scenarioQuery(func(provider *simProvider, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (any, error) {
			var name string

			err := starlark.UnpackArgs(b.Name(), args, kwargs, "name", &name)
			if err != nil {
				return nil, err
			}

			return provider.Project(context.Background(), name)
		}),
	}

	env := starlark.StringDict{}
	for name, fn := range builtins {
		env[name] = starlark.NewBuiltin(name, fn)
	}

	return env
}

// loadScenario compiles a scenario file and finds its settings and test functions.
func loadScenario(path string) (*scenario, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	env := scenarioBuiltins()

	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}

	prog, err := scriptlet.Compile(path, string(src), names)
	if err != nil {
		return nil, fmt.Errorf("Failed parsing scenario: %w", err)
	}

	thread := &starlark.Thread{Name: path, Print: func(_ *starlark.Thread, msg string) { fmt.Fprintln(os.Stderr, msg) }}

	globals, err := prog.Init(thread, env)
	if err != nil {
		return nil, fmt.Errorf("Failed loading scenario: %s", describeFailure(err))
	}

	// Tests can't change the file's globals, so one can't affect another.
	globals.Freeze()

	sc := &scenario{Path: path}

	settings := map[string]*string{"cluster": &sc.Cluster, "scriptlet": &sc.Scriptlet, "local_member": &sc.LocalMember}
	for name, setting := range settings {
		value, ok := globals[name]
		if !ok {
			continue
		}

		text, ok := starlark.AsString(value)
		if !ok {
			return nil, fmt.Errorf("Invalid %q in scenario %q, expected a string", name, path)
		}

		*setting = text
	}

	for name, value := range globals {
		fn, ok := value.(*starlark.Function)
		if !ok || !strings.HasPrefix(name, "test_") {
			continue
		}

		if fn.NumParams() != 0 {
			return nil, fmt.Errorf("Test function %q in scenario %q can't take arguments", name, path)
		}

		sc.Tests = append(sc.Tests, fn)
	}

	if len(sc.Tests) == 0 {
		return nil, fmt.Errorf("Scenario %q doesn't define any test_ function", path)
	}

	// Run the tests in the order they are written.
	sort.Slice(sc.Tests, func(i int, j int) bool {
		return sc.Tests[i].Position().Line < sc.Tests[j].Position().Line
	})

	return sc, nil
}

// describeFailure turns a Starlark error into a message pointing at the line it came from.
func describeFailure(err error) string {
	var evalErr *starlark.EvalError
	if !errors.As(err, &evalErr) {
		return err.Error()
	}

	// Find the innermost frame which is in the scenario itself rather than in a builtin.
	for i := len(evalErr.CallStack) - 1; i >= 0; i-- {
		pos := evalErr.CallStack[i].Pos
		if pos.Filename() != "<builtin>" && pos.Line > 0 {
			return fmt.Sprintf("%s:%d: %s", filepath.Base(pos.Filename()), pos.Line, evalErr.Msg)
		}
	}

	return evalErr.Msg
}

// runTest runs a test function against a simulator.
func runTest(ctx context.Context, fn *starlark.Function, sim *simulator) testReport {
	run := &testRun{ctx: ctx, sim: sim}

	thread := &starlark.Thread{Name: fn.Name(), Print: func(_ *starlark.Thread, msg string) { run.output = append(run.output, msg) }}
	thread.SetLocal(testRunKey, run)

	rep := testReport{Name: fn.Name()}

	_, err := starlark.Call(thread, fn, nil, nil)
	if err != nil {
		rep.Failure = describeFailure(err)
	}

	rep.Results = run.results
	rep.Output = run.output

	return rep
}

// run plays every test, each of them starting from the cluster as it was captured.
func (sc *scenario) run(ctx context.Context, clusterPath string, scriptletSrc string, localMember string) ([]testReport, error) {
	reports := make([]testReport, 0, len(sc.Tests))
	for _, fn := range sc.Tests {
		snap, err := loadSnapshot(clusterPath)
		if err != nil {
			return nil, err
		}

		sim, err := newSimulator(snap, scriptletSrc, localMember)
		if err != nil {
			return nil, err
		}

		reports = append(reports, runTest(ctx, fn, sim))
	}

	return reports, nil
}
