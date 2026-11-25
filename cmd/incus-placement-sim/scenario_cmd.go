package main

import (
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
	flagFormat      string
	flagColor       string
	flagVerbose     bool

	runs []docRun
}

func (c *cmdTest) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "test <scenario>..."
	cmd.Short = "Check a scriptlet against scenario files"
	cmd.Long = `Description:
  Check a scriptlet against scenario files

  A scenario is a Starlark file, the language scriptlets are written in.
  Each of its test_ functions starts from the cluster as it was captured,
  simulates some actions and checks what happened. The command fails if
  any test does, making it suitable for automated testing.

    cluster = "cluster.yaml"
    scriptlet = "placement.star"

    def test_databases_stay_apart():
        placed = evacuate("node2")
        assert_eq(placed["db2"].target, "node3")

        for member in get_cluster_members():
            count = get_instances_count(location = member.server_name)
            assert_true(count <= 3, member.server_name + " is too busy")

        db3 = create(name = "db3", type = "virtual-machine", profiles = ["default", "db"])
        assert_eq(db3.outcome, "rejected")

    def test_web_instances_move_freely():
        moved = move("web1", target = "@default")
        assert_true(moved.target in ["node1", "node3"])

  The cluster and scriptlet paths are relative to the scenario file and
  can be overridden with --cluster and --scriptlet. Without a scriptlet,
  the one captured with the cluster or the built-in placement is tested.

  Actions, which each return what happened to the instance:
    create(name, project, type, profiles, config, devices, target,
           architecture, image)
    move(instance, target, project)
    evacuate(member, heal)    returns a dict of placements by instance name

  A placement has these fields:
    outcome     scriptlet, builtin, validated, rejected or stays
    target      the cluster member the instance ends up on
    error       why the placement was rejected
    candidates  the cluster members the scriptlet could choose from
    excluded    the other cluster members, with the reason for each

  Checks, which end the test when they fail:
    assert_eq(got, want, msg)
    assert_true(cond, msg)
    fail(msg)

  The state of the cluster is read with the same functions as in a
  placement scriptlet:
    get_instances(project, location)
    get_instances_count(project, location, pending)
    get_cluster_members(group)
    get_cluster_member_resources(member_name)
    get_cluster_member_state(member_name)
    get_project(name)

  With --format json, the output is a document meant for other programs,
  with one run per test. Each run has the verdict of the test, the cluster,
  its state before and after, and every action with its placements.
`
	cmd.Example = `  incus-placement-sim test scenarios/*.star
  incus-placement-sim test evacuation.star -s new-placement.star -v`
	cmd.Args = cobra.MinimumNArgs(1)
	cmd.RunE = c.run

	cmd.Flags().StringVarP(&c.flagCluster, "cluster", "c", "", "Cluster snapshot file (overrides the scenario)")
	cmd.Flags().StringVarP(&c.flagScriptlet, "scriptlet", "s", "", "Instance placement scriptlet file (overrides the scenario)")
	cmd.Flags().StringVar(&c.flagLocalMember, "local-member", "", "Cluster member handling the requests (overrides the scenario)")
	cmd.Flags().StringVar(&c.flagFormat, "format", "text", "Output format (text or json)")
	cmd.Flags().StringVar(&c.flagColor, "color", "auto", "Color the text output (auto, always or never)")
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

// runScenario plays a scenario file and returns how many of its tests were run and failed.
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

	reports, err := sc.run(cmd.Context(), clusterPath, src, localMember)
	if err != nil {
		return 0, 0, err
	}

	failed := 0
	for _, rep := range reports {
		if rep.Failure != "" {
			failed++
		}

		// Keep everything that happened in the test for the JSON output.
		run := rep.Sim.document(rep.Name, logic)
		run.Scenario = path
		run.Test = &docTest{Passed: rep.Failure == "", Failure: rep.Failure, Output: append([]string{}, rep.Output...)}
		c.runs = append(c.runs, run)
	}

	if c.flagFormat == "json" {
		return len(reports), failed, nil
	}

	fmt.Println(pal.title.Sprint(path) + pal.dim.Sprintf(" (%s)", logic))

	for _, rep := range reports {
		status := pal.good.Sprint("PASS")
		if rep.Failure != "" {
			status = pal.bad.Sprint("FAIL")
		}

		fmt.Printf("  %s  %s\n", status, rep.Name)

		if rep.Failure != "" {
			fmt.Printf("        %s\n", rep.Failure)
		}

		for _, line := range rep.Output {
			fmt.Println(pal.dim.Sprintf("        %s", line))
		}

		if c.flagVerbose {
			for _, res := range rep.Results {
				printResult(os.Stdout, res, src != "", pal, "        ")
			}
		}
	}

	fmt.Println()

	return len(reports), failed, nil
}

func (c *cmdTest) run(cmd *cobra.Command, args []string) error {
	if c.flagFormat != "text" && c.flagFormat != "json" {
		return fmt.Errorf("Invalid format %q", c.flagFormat)
	}

	pal, err := newPalette(c.flagColor)
	if err != nil {
		return err
	}

	tests := 0
	failed := 0
	for _, path := range args {
		scenarioTests, scenarioFailed, err := c.runScenario(cmd, path, pal)
		if err != nil {
			return err
		}

		tests += scenarioTests
		failed += scenarioFailed
	}

	// The document is complete even when tests failed, the exit status telling which it was.
	if c.flagFormat == "json" {
		err = writeDocument(os.Stdout, "test", c.runs)
		if err != nil {
			return err
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d tests failed", failed, tests)
	}

	if c.flagFormat == "text" {
		fmt.Println(pal.good.Sprintf("All %d tests passed", tests))
	}

	return nil
}
