package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lxc/incus/v7/shared/api"
)

type cmdRun struct {
	global *cmdGlobal

	flagCluster     string
	flagScriptlet   string
	flagLocalMember string
	flagFormat      string
}

// report is the outcome of a simulated action.
type report struct {
	Results      []*result               `json:"results"`
	Distribution map[string]distribution `json:"distribution"`
}

// distribution is the instance count of a cluster member before and after an action.
type distribution struct {
	Before int `json:"before"`
	After  int `json:"after"`
}

func (c *cmdRun) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "run"
	cmd.Short = "Simulate an action against a cluster snapshot"
	cmd.Long = `Description:
  Simulate an action against a cluster snapshot

  The scriptlet is run exactly as the server would run it, with its
  queries answered from the snapshot rather than from a live cluster.

  When no scriptlet is passed, the one captured with the cluster is used.
  If there is none, the built-in placement logic is shown instead, which
  picks the candidate with the fewest instances (across all projects),
  regardless of CPU or memory usage.
`

	cmd.PersistentFlags().StringVarP(&c.flagCluster, "cluster", "c", "cluster.yaml", "Cluster snapshot file")
	cmd.PersistentFlags().StringVarP(&c.flagScriptlet, "scriptlet", "s", "", "Instance placement scriptlet file")
	cmd.PersistentFlags().StringVar(&c.flagLocalMember, "local-member", "", "Cluster member handling the request (defaults to the first member)")
	cmd.PersistentFlags().StringVar(&c.flagFormat, "format", "text", "Output format (text or json)")

	// create sub-command.
	createCmd := cmdRunCreate{run: c}
	cmd.AddCommand(createCmd.command())

	// move sub-command.
	moveCmd := cmdRunMove{run: c}
	cmd.AddCommand(moveCmd.command())

	// evacuate sub-command.
	evacuateCmd := cmdRunEvacuate{run: c}
	cmd.AddCommand(evacuateCmd.command())

	return cmd
}

// simulator loads the snapshot and scriptlet and returns a ready to use simulator.
func (c *cmdRun) simulator() (*simulator, error) {
	if c.flagFormat != "text" && c.flagFormat != "json" {
		return nil, fmt.Errorf("Invalid format %q", c.flagFormat)
	}

	snap, err := loadSnapshot(c.flagCluster)
	if err != nil {
		return nil, err
	}

	src := snap.Server.Config["instances.placement.scriptlet"]
	if c.flagScriptlet != "" {
		content, err := os.ReadFile(c.flagScriptlet)
		if err != nil {
			return nil, err
		}

		src = string(content)
	} else if src != "" {
		fmt.Fprintln(os.Stderr, "Using the scriptlet captured with the cluster")
	} else {
		fmt.Fprintln(os.Stderr, "No scriptlet provided, showing the built-in placement (candidate with the fewest instances)")
	}

	return newSimulator(snap, src, c.flagLocalMember)
}

// counts returns the number of instances on each cluster member.
func counts(sim *simulator) map[string]int {
	result := map[string]int{}
	for _, member := range sim.snap.Members {
		result[member.ServerName] = sim.instancesCount("", member.ServerName)
	}

	return result
}

// execute runs an action and prints its report.
func (c *cmdRun) execute(action func(sim *simulator) ([]*result, error)) error {
	sim, err := c.simulator()
	if err != nil {
		return err
	}

	before := counts(sim)

	results, err := action(sim)
	if err != nil {
		return err
	}

	after := counts(sim)

	rep := report{Results: results, Distribution: map[string]distribution{}}
	for name := range before {
		rep.Distribution[name] = distribution{Before: before[name], After: after[name]}
	}

	if c.flagFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(rep)
	}

	return printReport(os.Stdout, sim, rep)
}

// instancesLabel formats an instance count.
func instancesLabel(count int) string {
	if count == 1 {
		return "1 instance"
	}

	return fmt.Sprintf("%d instances", count)
}

// printReport writes a human readable report.
func printReport(w io.Writer, sim *simulator, rep report) error {
	for _, res := range rep.Results {
		header := fmt.Sprintf("[%s] %s/%s", res.Reason, res.Project, res.Instance)
		if res.Source != "" {
			header += " (on " + res.Source + ")"
		}

		_, _ = fmt.Fprintln(w, header)

		if res.Candidates != nil {
			entries := make([]string, 0, len(res.Candidates))
			for _, name := range res.Candidates {
				entries = append(entries, fmt.Sprintf("%s (%s)", name, instancesLabel(res.InstanceCounts[name])))
			}

			_, _ = fmt.Fprintf(w, "  candidates: %s\n", strings.Join(entries, ", "))
		}

		for i, entry := range res.Excluded {
			label := "excluded:"
			if i > 0 {
				label = ""
			}

			_, _ = fmt.Fprintf(w, "  %-11s %s (%s)\n", label, entry.Member, entry.Reason)
		}

		for _, entry := range res.Trace {
			_, _ = fmt.Fprintf(w, "    %-5s %s\n", entry.Kind, entry.Message)
		}

		switch res.Outcome {
		case outcomeScriptlet:
			_, _ = fmt.Fprintf(w, "  result:     %s (set by scriptlet)\n", res.Target)
		case outcomeBuiltin:
			_, _ = fmt.Fprintf(w, "  result:     %s (built-in: %s)\n", res.Target, res.Detail)
		case outcomeValidated:
			_, _ = fmt.Fprintf(w, "  result:     %s (accepted: %s)\n", res.Target, res.Detail)
		case outcomeStays:
			_, _ = fmt.Fprintf(w, "  result:     stays on %s (%s)\n", res.Target, res.Detail)
		case outcomeRejected:
			_, _ = fmt.Fprintf(w, "  result:     REJECTED: %s\n", res.Error)
		}

		if res.Duration != "" {
			_, _ = fmt.Fprintf(w, "  duration:   %s\n", res.Duration)
		}

		_, _ = fmt.Fprintln(w)
	}

	// Show the instance distribution in member order.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MEMBER\tSTATUS\tINSTANCES BEFORE\tINSTANCES AFTER")
	for _, member := range sim.snap.Members {
		dist := rep.Distribution[member.ServerName]
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", member.ServerName, member.Status, dist.Before, dist.After)
	}

	return tw.Flush()
}

// parseKeyValues parses a list of key=value entries.
func parseKeyValues(entries []string) (map[string]string, error) {
	result := map[string]string{}
	for _, entry := range entries {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("Invalid key=value entry %q", entry)
		}

		result[key] = value
	}

	return result, nil
}

type cmdRunCreate struct {
	run *cmdRun

	flagProject    string
	flagName       string
	flagType       string
	flagProfiles   []string
	flagNoProfiles bool
	flagConfig     []string
	flagDevices    []string
	flagTarget     string
	flagArch       string
	flagImage      string
}

func (c *cmdRunCreate) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "create"
	cmd.Short = "Simulate the creation of an instance"
	cmd.Long = `Description:
  Simulate the creation of an instance

  The scriptlet is called with the "new" reason.
`
	cmd.Example = `  incus-placement run create -c cluster.yaml -s placement.star --name web1 --config limits.memory=4GiB
  incus-placement run create -c cluster.yaml -s placement.star --type virtual-machine --target @gpu`
	cmd.Args = cobra.NoArgs
	cmd.RunE = c.runE

	cmd.Flags().StringVar(&c.flagProject, "project", "", "Project of the instance (defaults to the only project in the snapshot or \"default\")")
	cmd.Flags().StringVar(&c.flagName, "name", "new-instance", "Name of the instance")
	cmd.Flags().StringVar(&c.flagType, "type", string(api.InstanceTypeContainer), "Instance type (container or virtual-machine)")
	cmd.Flags().StringArrayVarP(&c.flagProfiles, "profile", "p", nil, "Profile to apply to the instance (defaults to \"default\")")
	cmd.Flags().BoolVar(&c.flagNoProfiles, "no-profiles", false, "Create the instance without any profile")
	cmd.Flags().StringArrayVar(&c.flagConfig, "config", nil, "Instance configuration as key=value")
	cmd.Flags().StringArrayVarP(&c.flagDevices, "device", "d", nil, "Device override as name,key=value[,key=value]")
	cmd.Flags().StringVar(&c.flagTarget, "target", "", "Cluster member or @group requested for the instance")
	cmd.Flags().StringVar(&c.flagArch, "arch", "", "Architecture of the instance image (e.g. x86_64)")
	cmd.Flags().StringVar(&c.flagImage, "image", "", "Image alias shown to the scriptlet as the instance source")

	return cmd
}

func (c *cmdRunCreate) runE(cmd *cobra.Command, args []string) error {
	opts := createOptions{
		Project:      c.flagProject,
		Name:         c.flagName,
		Type:         c.flagType,
		Profiles:     c.flagProfiles,
		Target:       c.flagTarget,
		Architecture: c.flagArch,
		Image:        c.flagImage,
	}

	if opts.Type == "vm" {
		opts.Type = string(api.InstanceTypeVM)
	}

	if opts.Type != string(api.InstanceTypeContainer) && opts.Type != string(api.InstanceTypeVM) {
		return fmt.Errorf("Invalid instance type %q", opts.Type)
	}

	if c.flagNoProfiles {
		if len(c.flagProfiles) > 0 {
			return errors.New("Can't use --profile and --no-profiles together")
		}

		opts.Profiles = []string{}
	}

	var err error

	opts.Config, err = parseKeyValues(c.flagConfig)
	if err != nil {
		return err
	}

	opts.Devices = map[string]map[string]string{}
	for _, entry := range c.flagDevices {
		fields := strings.Split(entry, ",")
		if len(fields) < 2 || fields[0] == "" {
			return fmt.Errorf("Invalid device override %q", entry)
		}

		opts.Devices[fields[0]], err = parseKeyValues(fields[1:])
		if err != nil {
			return err
		}
	}

	return c.run.execute(func(sim *simulator) ([]*result, error) {
		opts.Project = sim.defaultProject(opts.Project)

		res, err := sim.create(cmd.Context(), opts)
		if err != nil {
			return nil, err
		}

		return []*result{res}, nil
	})
}

type cmdRunMove struct {
	run *cmdRun

	flagProject string
	flagTarget  string
}

func (c *cmdRunMove) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "move <instance>"
	cmd.Short = "Simulate moving an instance within the cluster"
	cmd.Long = `Description:
  Simulate moving an instance within the cluster

  The scriptlet is called with the "relocation" reason. When the target is
  a specific cluster member, the scriptlet can only accept or reject it.
`
	cmd.Example = `  incus-placement run move -c cluster.yaml -s placement.star web1 --target @default
  incus-placement run move -c cluster.yaml -s placement.star web1 --target node2`
	cmd.Args = cobra.ExactArgs(1)
	cmd.RunE = c.runE

	cmd.Flags().StringVar(&c.flagProject, "project", "", "Project of the instance (defaults to the only project in the snapshot or \"default\")")
	cmd.Flags().StringVar(&c.flagTarget, "target", "", "Cluster member or @group to move the instance to")

	return cmd
}

func (c *cmdRunMove) runE(cmd *cobra.Command, args []string) error {
	return c.run.execute(func(sim *simulator) ([]*result, error) {
		res, err := sim.move(cmd.Context(), sim.defaultProject(c.flagProject), args[0], c.flagTarget)
		if err != nil {
			return nil, err
		}

		return []*result{res}, nil
	})
}

type cmdRunEvacuate struct {
	run *cmdRun

	flagHeal bool
}

func (c *cmdRunEvacuate) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "evacuate <member>"
	cmd.Short = "Simulate the evacuation of a cluster member"
	cmd.Long = `Description:
  Simulate the evacuation of a cluster member

  The scriptlet is called with the "evacuation" reason for every instance
  that gets migrated away from the cluster member. Each move is applied
  before the next instance is placed.

  With --heal, the member is treated as having gone offline instead, as
  happens with automatic healing (cluster.healing_threshold).
`
	cmd.Args = cobra.ExactArgs(1)
	cmd.RunE = c.runE

	cmd.Flags().BoolVar(&c.flagHeal, "heal", false, "Simulate automatic healing of an offline member")

	return cmd
}

func (c *cmdRunEvacuate) runE(cmd *cobra.Command, args []string) error {
	return c.run.execute(func(sim *simulator) ([]*result, error) {
		return sim.evacuate(cmd.Context(), args[0], c.flagHeal)
	})
}
