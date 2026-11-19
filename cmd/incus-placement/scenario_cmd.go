package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

type cmdTest struct {
	global *cmdGlobal

	flagCluster     string
	flagScriptlet   string
	flagLocalMember string
	flagColor       string
	flagVerbose     bool
}

func (c *cmdTest) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "test <scenario>..."
	cmd.Short = "Check a scriptlet against scenario files"
	cmd.Long = `Description:
  Check a scriptlet against scenario files

  A scenario is a YAML file listing actions to simulate one after the
  other, each with the outcome expected from it. The state of the cluster
  carries over from one step to the next. The command fails if any
  expectation isn't met, making it suitable for automated testing.

    description: Database instances stay apart
    cluster: cluster.yaml
    scriptlet: placement.star
    steps:
      - evacuate: {member: node2}
        expect:
          target_not: [node4]
          instances:
            db2: {outcome: scriptlet, target: node3}
          members:
            "*": {max_instances: 3}
          apart: [db1, db2]
      - create: {name: db3, type: virtual-machine, profiles: [default, db]}
        expect: {outcome: rejected, error: No cluster member}
      - move: {instance: web1, target: "@gpu"}
        expect: {target_in: [node3, node4]}

  The cluster and scriptlet paths are relative to the scenario file and
  can be overridden with --cluster and --scriptlet. Without a scriptlet,
  the one captured with the cluster or the built-in placement is tested.

  Steps are one of:
    create    name, project, type, profiles, config, devices, target,
              architecture, image
    move      instance, project, target
    evacuate  member, heal

  Expectations are checked against every instance placed by the step:
    outcome     scriptlet, builtin, validated, rejected or stays
    target      the cluster member the instance ends up on
    target_in   a list of acceptable cluster members
    target_not  a list of unacceptable cluster members
    error       text found in the rejection message
    instances   the same expectations for specific instances, which
                replace the step wide ones for those instances
    members     instances, min_instances or max_instances per cluster
                member once the step is done, "*" matching all others
    apart       instances which must all be on different cluster members
`
	cmd.Example = `  incus-placement test scenarios/*.yaml
  incus-placement test evacuation.yaml -s new-placement.star -v`
	cmd.Args = cobra.MinimumNArgs(1)
	cmd.RunE = c.run

	cmd.Flags().StringVarP(&c.flagCluster, "cluster", "c", "", "Cluster snapshot file (overrides the scenario)")
	cmd.Flags().StringVarP(&c.flagScriptlet, "scriptlet", "s", "", "Instance placement scriptlet file (overrides the scenario)")
	cmd.Flags().StringVar(&c.flagLocalMember, "local-member", "", "Cluster member handling the requests (overrides the scenario)")
	cmd.Flags().StringVar(&c.flagColor, "color", "auto", "Color the output (auto, always or never)")
	cmd.Flags().BoolVarP(&c.flagVerbose, "verbose", "v", false, "Show the details of every placement")

	return cmd
}

// relativeTo resolves a path found in a scenario file.
func relativeTo(scenarioPath string, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(filepath.Dir(scenarioPath), path)
}

// runScenario plays a scenario file and returns how many of its steps were run and failed.
func (c *cmdTest) runScenario(cmd *cobra.Command, path string, pal *palette) (int, int, error) {
	sc, err := loadScenario(path)
	if err != nil {
		return 0, 0, err
	}

	clusterPath := relativeTo(path, sc.Cluster)
	if c.flagCluster != "" {
		clusterPath = c.flagCluster
	}

	if clusterPath == "" {
		return 0, 0, fmt.Errorf("Scenario %q doesn't name a cluster snapshot and --cluster wasn't passed", path)
	}

	snap, err := loadSnapshot(clusterPath)
	if err != nil {
		return 0, 0, err
	}

	// Pick the scriptlet from the command line, the scenario or the snapshot (in that order).
	scriptletPath := relativeTo(path, sc.Scriptlet)
	if c.flagScriptlet != "" {
		scriptletPath = c.flagScriptlet
	}

	src := snap.Server.Config["instances.placement.scriptlet"]
	logic := builtinVariant + " placement"
	if src != "" {
		logic = "captured scriptlet"
	}

	if scriptletPath != "" {
		content, err := os.ReadFile(scriptletPath)
		if err != nil {
			return 0, 0, err
		}

		src = string(content)
		logic = scriptletPath
	}

	localMember := sc.LocalMember
	if c.flagLocalMember != "" {
		localMember = c.flagLocalMember
	}

	sim, err := newSimulator(snap, src, localMember)
	if err != nil {
		return 0, 0, err
	}

	title := path
	if sc.Description != "" {
		title += ": " + sc.Description
	}

	fmt.Println(pal.title.Sprint(title) + pal.dim.Sprintf(" (%s)", logic))

	reports := sc.run(cmd.Context(), sim)

	failed := 0
	for i, rep := range reports {
		status := pal.good.Sprint("PASS")
		if len(rep.Failures) > 0 {
			status = pal.bad.Sprint("FAIL")
			failed++
		}

		fmt.Printf("  %s  %d. %s\n", status, i+1, rep.Name)

		for _, failure := range rep.Failures {
			fmt.Printf("        %s\n", failure)
		}

		if c.flagVerbose {
			for _, res := range rep.Results {
				printResult(os.Stdout, res, sim.hasScriptlet, pal, "        ")
			}
		}
	}

	return len(reports), failed, nil
}

func (c *cmdTest) run(cmd *cobra.Command, args []string) error {
	pal, err := newPalette(c.flagColor)
	if err != nil {
		return err
	}

	steps := 0
	failed := 0
	for _, path := range args {
		scenarioSteps, scenarioFailed, err := c.runScenario(cmd, path, pal)
		if err != nil {
			return err
		}

		steps += scenarioSteps
		failed += scenarioFailed

		fmt.Println()
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d steps didn't meet their expectations", failed, steps)
	}

	if steps == 0 {
		return errors.New("No step was run")
	}

	fmt.Println(pal.good.Sprintf("All %d steps met their expectations", steps))

	return nil
}
