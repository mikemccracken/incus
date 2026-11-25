package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/units"
)

type cmdRun struct {
	global *cmdGlobal

	flagCluster     string
	flagScriptlets  []string
	flagCompare     bool
	flagDetails     bool
	flagLocalMember string
	flagFormat      string
	flagColor       string
}

// report is the outcome of a simulated action.
type report struct {
	Results      []*result
	Distribution map[string]distribution
}

// distribution is the instance count and committed resources of a cluster member before and after an action.
type distribution struct {
	Before                int
	After                 int
	CommittedCPUsBefore   uint64
	CommittedCPUsAfter    uint64
	CommittedMemoryBefore uint64
	CommittedMemoryAfter  uint64
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

  Passing several scriptlets, or --compare, runs the action once for each
  of them and once with the built-in logic, always starting from the same
  snapshot, then shows where every instance ends up side by side.
  Add --details to also get the candidates, exclusions and scriptlet
  activity of every placement, for each of the scriptlets in turn.

  With --format json, the output is a document meant for other programs,
  with one run per placement logic. Each run has the cluster, its state
  before and after, and every placement in order: what was considered,
  what the scriptlet did, what was decided and the state it led to.
`

	cmd.PersistentFlags().StringVarP(&c.flagCluster, "cluster", "c", "cluster.yaml", "Cluster snapshot file")
	cmd.PersistentFlags().StringArrayVarP(&c.flagScriptlets, "scriptlet", "s", nil, "Instance placement scriptlet file (repeat to compare several)")
	cmd.PersistentFlags().BoolVar(&c.flagCompare, "compare", false, "Compare the scriptlet with the built-in placement")
	cmd.PersistentFlags().BoolVar(&c.flagDetails, "details", false, "Show the details of every placement before the comparison")
	cmd.PersistentFlags().StringVar(&c.flagLocalMember, "local-member", "", "Cluster member handling the request (defaults to the first member)")
	cmd.PersistentFlags().StringVar(&c.flagFormat, "format", "text", "Output format (text or json)")
	cmd.PersistentFlags().StringVar(&c.flagColor, "color", "auto", "Color the text output (auto, always or never)")

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

// builtinVariant is the name given to the built-in placement logic.
const builtinVariant = "built-in"

// variant is a placement logic to simulate, the built-in one when it has no scriptlet.
type variant struct {
	Name      string
	Scriptlet string
}

// variantReport is the outcome of a simulated action for one placement logic.
type variantReport struct {
	report

	Name string
}

// comparison is the outcome of a simulated action for several placement logics.
type comparison struct {
	Variants []variantReport
}

// variants returns the placement logics to simulate and whether to show them side by side.
func (c *cmdRun) variants() ([]variant, bool, error) {
	if c.flagFormat != "text" && c.flagFormat != "json" {
		return nil, false, fmt.Errorf("Invalid format %q", c.flagFormat)
	}

	snap, err := loadSnapshot(c.flagCluster)
	if err != nil {
		return nil, false, err
	}

	captured := snap.Server.Config["instances.placement.scriptlet"]

	names := []string{builtinVariant, "captured"}
	scriptlets := make([]variant, 0, len(c.flagScriptlets))
	for _, path := range c.flagScriptlets {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, false, err
		}

		// Fallback to the full path when two files share a name.
		name := filepath.Base(path)
		if slices.Contains(names, name) {
			name = path
		}

		names = append(names, name)
		scriptlets = append(scriptlets, variant{Name: name, Scriptlet: string(content)})
	}

	if !c.flagCompare && len(scriptlets) < 2 {
		if len(scriptlets) == 1 {
			return scriptlets, false, nil
		}

		if captured != "" {
			fmt.Fprintln(os.Stderr, "Using the scriptlet captured with the cluster")

			return []variant{{Name: "captured", Scriptlet: captured}}, false, nil
		}

		fmt.Fprintln(os.Stderr, "No scriptlet provided, showing the built-in placement (candidate with the fewest instances)")

		return []variant{{Name: builtinVariant}}, false, nil
	}

	// A comparison starts with the built-in logic, then what the cluster currently runs.
	all := []variant{{Name: builtinVariant}}
	if captured != "" {
		all = append(all, variant{Name: "captured", Scriptlet: captured})
	}

	return append(all, scriptlets...), true, nil
}

// simulate runs an action with the given placement logic against a fresh copy of the snapshot.
func (c *cmdRun) simulate(v variant, action func(sim *simulator) ([]*result, error)) (*simulator, report, error) {
	snap, err := loadSnapshot(c.flagCluster)
	if err != nil {
		return nil, report{}, err
	}

	sim, err := newSimulator(snap, v.Scriptlet, c.flagLocalMember)
	if err != nil {
		return nil, report{}, err
	}

	before := sim.usage()

	results, err := action(sim)
	if err != nil {
		return nil, report{}, err
	}

	after := sim.usage()

	rep := report{Results: results, Distribution: map[string]distribution{}}
	for name := range before {
		rep.Distribution[name] = distribution{
			Before:                before[name].Instances,
			After:                 after[name].Instances,
			CommittedCPUsBefore:   before[name].CPUs,
			CommittedCPUsAfter:    after[name].CPUs,
			CommittedMemoryBefore: before[name].Memory,
			CommittedMemoryAfter:  after[name].Memory,
		}
	}

	return sim, rep, nil
}

// execute runs an action and prints its report.
func (c *cmdRun) execute(action func(sim *simulator) ([]*result, error)) error {
	pal, err := newPalette(c.flagColor)
	if err != nil {
		return err
	}

	variants, compare, err := c.variants()
	if err != nil {
		return err
	}

	// The JSON output has one run per placement logic, each with everything that happened.
	if c.flagFormat == "json" {
		runs := make([]docRun, 0, len(variants))
		for _, v := range variants {
			sim, _, err := c.simulate(v, action)
			if err != nil {
				return fmt.Errorf("%s: %w", v.Name, err)
			}

			runs = append(runs, sim.document(v.Name, v.Name))
		}

		return writeDocument(os.Stdout, "run", runs)
	}

	if !compare {
		sim, rep, err := c.simulate(variants[0], action)
		if err != nil {
			return err
		}

		return printReport(os.Stdout, sim, rep, pal)
	}

	// The scriptlet is a process wide setting, so the variants are run one after the other.
	var sim *simulator

	cmp := comparison{}
	for _, v := range variants {
		var rep report

		sim, rep, err = c.simulate(v, action)
		if err != nil {
			return fmt.Errorf("%s: %w", v.Name, err)
		}

		cmp.Variants = append(cmp.Variants, variantReport{report: rep, Name: v.Name})

		// Show how this variant got to its placements.
		if c.flagDetails {
			fmt.Println(pal.title.Sprintf("=== %s ===", v.Name))

			for _, res := range rep.Results {
				printResult(os.Stdout, res, sim.hasScriptlet, pal, "")
				fmt.Println()
			}
		}
	}

	if c.flagDetails {
		fmt.Println(pal.title.Sprint("=== comparison ==="))
	}

	printComparison(os.Stdout, sim, cmp, pal)

	return nil
}

// outcomeCell summarizes a placement for the comparison table.
func outcomeCell(res *result, baseline *result, pal *palette) cell {
	if res == nil {
		return cell{text: "-"}
	}

	switch res.Outcome {
	case outcomeRejected:
		return cell{text: "REJECTED", style: pal.bad}
	case outcomeStays:
		return cell{text: "stays", style: pal.caution}
	}

	// Mark the placements where a scriptlet left the choice to the built-in logic.
	c := cell{text: res.Target}
	if res == baseline {
		return c
	}

	if res.Outcome == outcomeBuiltin {
		c.text += "*"
	}

	// Highlight what differs from the built-in logic.
	if res.Target != baseline.Target {
		c.style = pal.tag
	}

	return c
}

// printComparison writes where each instance ends up for every placement logic.
func printComparison(w io.Writer, sim *simulator, cmp comparison, pal *palette) {
	key := func(res *result) string {
		return res.Project + "/" + res.Instance
	}

	// Index the results of each variant by instance.
	indexes := make([]map[string]*result, 0, len(cmp.Variants))
	for _, v := range cmp.Variants {
		index := map[string]*result{}
		for _, res := range v.Results {
			index[key(res)] = res
		}

		indexes = append(indexes, index)
	}

	header := []cell{{text: "INSTANCE", style: pal.title}}
	for _, v := range cmp.Variants {
		header = append(header, cell{text: v.Name, style: pal.title})
	}

	fallback := false
	rows := [][]cell{header}
	for _, res := range cmp.Variants[0].Results {
		row := []cell{{text: key(res)}}
		for i := range cmp.Variants {
			c := outcomeCell(indexes[i][key(res)], res, pal)
			fallback = fallback || strings.HasSuffix(c.text, "*")
			row = append(row, c)
		}

		rows = append(rows, row)
	}

	printTable(w, rows)

	if fallback {
		_, _ = fmt.Fprintln(w, pal.dim.Sprint("* the scriptlet didn't call set_target, the candidate with the fewest instances was used"))
	}

	// Explain the rejections.
	for _, v := range cmp.Variants {
		for _, res := range v.Results {
			if res.Outcome == outcomeRejected {
				_, _ = fmt.Fprintf(w, "%s %s\n", pal.bad.Sprintf("%s rejected %s:", v.Name, key(res)), res.Error)
			}
		}
	}

	_, _ = fmt.Fprintln(w)

	// Show the members with their resources and resulting instance count, in member order.
	_, _ = fmt.Fprintln(w, pal.dim.Sprint("Instances on each member:"))

	header = append(memberHeader(pal), cell{text: "BEFORE", style: pal.title})
	for _, v := range cmp.Variants {
		header = append(header, cell{text: v.Name, style: pal.title})
	}

	rows = [][]cell{header}
	for i := range sim.snap.Members {
		member := &sim.snap.Members[i]
		baseline := cmp.Variants[0].Distribution[member.ServerName]

		row := append(memberCells(member, pal), cell{text: strconv.Itoa(baseline.Before)})
		for _, v := range cmp.Variants {
			after := v.Distribution[member.ServerName].After

			c := cell{text: strconv.Itoa(after)}
			if after != baseline.After {
				c.style = pal.tag
			}

			row = append(row, c)
		}

		rows = append(rows, row)
	}

	printTable(w, rows)

	// Then what those instances add up to.
	committedTable(w, sim, cmp, pal, "CPUs committed on each member:", "CPUS", 3, func(dist distribution) (string, string) {
		return strconv.FormatUint(dist.CommittedCPUsBefore, 10), strconv.FormatUint(dist.CommittedCPUsAfter, 10)
	})

	committedTable(w, sim, cmp, pal, "Memory committed on each member:", "MEMORY", 4, func(dist distribution) (string, string) {
		return sizeLabel(dist.CommittedMemoryBefore), sizeLabel(dist.CommittedMemoryAfter)
	})

	_, _ = fmt.Fprintln(w, pal.dim.Sprint(memberLegend))
}

// committedTable writes what a member's instances add up to for every placement logic, next to its capacity.
func committedTable(w io.Writer, sim *simulator, cmp comparison, pal *palette, title string, capacity string, capacityColumn int, values func(dist distribution) (string, string)) {
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, pal.dim.Sprint(title))

	header := []cell{{text: "MEMBER", style: pal.title}, {text: capacity, style: pal.title}, {text: "BEFORE", style: pal.title}}
	for _, v := range cmp.Variants {
		header = append(header, cell{text: v.Name, style: pal.title})
	}

	rows := [][]cell{header}
	for i := range sim.snap.Members {
		member := &sim.snap.Members[i]

		before, baseline := values(cmp.Variants[0].Distribution[member.ServerName])

		row := []cell{{text: member.ServerName}, memberCells(member, pal)[capacityColumn], {text: before}}
		for _, v := range cmp.Variants {
			_, after := values(v.Distribution[member.ServerName])

			c := cell{text: after}
			if after != baseline {
				c.style = pal.tag
			}

			row = append(row, c)
		}

		rows = append(rows, row)
	}

	printTable(w, rows)
}

// memberLegend explains the resource columns of the member tables.
const memberLegend = "CPUS, MEMORY and MEMORY USED come from the member when it was captured, COMMITTED is the sum of the limits of its instances."

// sizeLabel formats a size in bytes.
func sizeLabel(size uint64) string {
	return units.GetByteSizeStringIEC(int64(size), 0)
}

// memberHeader returns the header of the columns describing a cluster member.
func memberHeader(pal *palette) []cell {
	header := []cell{}
	for _, name := range []string{"MEMBER", "STATUS", "ARCH", "CPUS", "MEMORY", "MEMORY USED"} {
		header = append(header, cell{text: name, style: pal.title})
	}

	return header
}

// memberCells returns the columns describing a cluster member and its resources.
func memberCells(member *SnapshotMember, pal *palette) []cell {
	status := cell{text: member.Status}
	if member.Status != "Online" {
		status.style = pal.caution
	}

	// Resources are missing for the members which couldn't be reached during the capture.
	cpus, memory, used := "-", "-", "-"
	if member.Resources != nil {
		cpus = strconv.FormatUint(member.Resources.CPU.Total, 10)
		memory = sizeLabel(member.Resources.Memory.Total)
		used = sizeLabel(member.Resources.Memory.Used)
	}

	return []cell{{text: member.ServerName}, status, {text: member.Architecture}, {text: cpus}, {text: memory}, {text: used}}
}

// changeCell shows a value, or both values if the action changed it.
func changeCell(before string, after string, pal *palette) cell {
	if before == after {
		return cell{text: before}
	}

	return cell{text: before + " -> " + after, style: pal.title}
}

// instancesLabel formats an instance count.
func instancesLabel(count int) string {
	if count == 1 {
		return "1 instance"
	}

	return fmt.Sprintf("%d instances", count)
}

// printResult writes the details of a single placement.
func printResult(w io.Writer, res *result, hasScriptlet bool, pal *palette, indent string) {
	label := func(name string) string {
		return pal.dim.Sprintf("%-11s", name)
	}

	header := pal.tag.Sprintf("[%s]", res.Reason) + " " + pal.title.Sprintf("%s/%s", res.Project, res.Instance)
	if res.Source != "" {
		header += pal.dim.Sprintf(" (on %s)", res.Source)
	}

	_, _ = fmt.Fprintln(w, indent+header)

	if res.Candidates != nil {
		entries := make([]string, 0, len(res.Candidates))
		for _, name := range res.Candidates {
			entries = append(entries, name+pal.dim.Sprintf(" (%s)", instancesLabel(res.InstanceCounts[name])))
		}

		_, _ = fmt.Fprintf(w, "%s  %s %s\n", indent, label("candidates:"), strings.Join(entries, ", "))
	}

	for i, entry := range res.Excluded {
		name := "excluded:"
		if i > 0 {
			name = ""
		}

		_, _ = fmt.Fprintf(w, "%s  %s %s%s\n", indent, label(name), entry.Member, pal.dim.Sprintf(" (%s)", entry.Reason))
	}

	for _, entry := range res.Trace {
		_, _ = fmt.Fprintln(w, indent+pal.trace(entry.label()).Sprintf("    %-5s %s", entry.label(), entry.Message))
	}

	var outcome string

	switch res.Outcome {
	case outcomeScriptlet:
		outcome = pal.good.Sprint(res.Target) + pal.dim.Sprint(" (set by scriptlet)")
	case outcomeBuiltin:
		// A scriptlet which didn't pick a target is worth a second look.
		style := pal.good
		if hasScriptlet {
			style = pal.caution
		}

		outcome = style.Sprint(res.Target) + pal.dim.Sprintf(" (built-in: %s)", res.Detail)
	case outcomeValidated:
		outcome = pal.good.Sprint(res.Target) + pal.dim.Sprintf(" (accepted: %s)", res.Detail)
	case outcomeStays:
		outcome = pal.caution.Sprintf("stays on %s", res.Target) + pal.dim.Sprintf(" (%s)", res.Detail)
	case outcomeRejected:
		outcome = pal.bad.Sprintf("REJECTED: %s", res.Error)
	}

	_, _ = fmt.Fprintf(w, "%s  %s %s\n", indent, label("result:"), outcome)

	if res.Duration != "" {
		_, _ = fmt.Fprintln(w, indent+pal.dim.Sprintf("  %-11s %s", "duration:", res.Duration))
	}
}

// printReport writes a human readable report.
func printReport(w io.Writer, sim *simulator, rep report, pal *palette) error {
	for _, res := range rep.Results {
		printResult(w, res, sim.hasScriptlet, pal, "")

		_, _ = fmt.Fprintln(w)
	}

	// Show the members with their resources and what the action changed, in member order.
	header := memberHeader(pal)
	for _, name := range []string{"INSTANCES", "COMMITTED CPUS", "COMMITTED MEMORY"} {
		header = append(header, cell{text: name, style: pal.title})
	}

	rows := [][]cell{header}
	for i := range sim.snap.Members {
		member := &sim.snap.Members[i]
		dist := rep.Distribution[member.ServerName]

		row := memberCells(member, pal)
		row = append(row, changeCell(strconv.Itoa(dist.Before), strconv.Itoa(dist.After), pal))
		row = append(row, changeCell(strconv.FormatUint(dist.CommittedCPUsBefore, 10), strconv.FormatUint(dist.CommittedCPUsAfter, 10), pal))
		row = append(row, changeCell(sizeLabel(dist.CommittedMemoryBefore), sizeLabel(dist.CommittedMemoryAfter), pal))

		rows = append(rows, row)
	}

	printTable(w, rows)

	_, _ = fmt.Fprintln(w, pal.dim.Sprint(memberLegend))
	_, _ = fmt.Fprintln(w, pal.dim.Sprint("Values which changed are shown as before -> after."))

	return nil
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
	cmd.Example = `  incus-placement-sim run create -c cluster.yaml -s placement.star --name web1 --config limits.memory=4GiB
  incus-placement-sim run create -c cluster.yaml -s placement.star --type virtual-machine --target @gpu`
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
	cmd.Example = `  incus-placement-sim run move -c cluster.yaml -s placement.star web1 --target @default
  incus-placement-sim run move -c cluster.yaml -s placement.star web1 --target node2`
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
