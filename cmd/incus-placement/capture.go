package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/cliconfig"
)

type cmdCapture struct {
	global *cmdGlobal

	flagOutput   string
	flagNoRedact bool
}

func (c *cmdCapture) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "capture [<remote>:]"
	cmd.Short = "Capture a description of a cluster"
	cmd.Long = `Description:
  Capture a description of a cluster

  This records everything the instance placement scriptlet can see about a
  cluster (members, resources, groups, projects, profiles and instances)
  into a snapshot file that can then be used with the "run" command.

  Only read access to the cluster is needed and nothing gets modified.
  Configuration keys which commonly hold secrets (cloud-init and
  environment) have their value redacted unless --no-redact is passed.
`
	cmd.Args = cobra.MaximumNArgs(1)
	cmd.RunE = c.run

	cmd.Flags().StringVarP(&c.flagOutput, "output", "o", "cluster.yaml", "Snapshot file to write (- for standard output)")
	cmd.Flags().BoolVar(&c.flagNoRedact, "no-redact", false, "Keep the value of sensitive configuration keys")

	return cmd
}

func (c *cmdCapture) run(cmd *cobra.Command, args []string) error {
	conf, err := cliconfig.LoadConfig("")
	if err != nil {
		return err
	}

	remote := conf.DefaultRemote
	if len(args) > 0 {
		remote = strings.TrimSuffix(args[0], ":")
	}

	server, err := conf.GetInstanceServer(remote)
	if err != nil {
		return err
	}

	if !server.IsClustered() {
		return fmt.Errorf("Remote %q isn't clustered", remote)
	}

	snap := &Snapshot{
		Version:    snapshotVersion,
		CapturedAt: time.Now().UTC().Truncate(time.Second),
	}

	// Server details.
	serverInfo, _, err := server.GetServer()
	if err != nil {
		return fmt.Errorf("Failed getting server details: %w", err)
	}

	snap.Server.Version = serverInfo.Environment.ServerVersion
	snap.Server.Config = map[string]string{}
	for _, key := range serverConfigKeys {
		value, ok := serverInfo.Config[key]
		if ok {
			snap.Server.Config[key] = value
		}
	}

	// Cluster members.
	members, err := server.GetClusterMembers()
	if err != nil {
		return fmt.Errorf("Failed getting cluster members: %w", err)
	}

	for _, member := range members {
		entry := SnapshotMember{ClusterMember: member}

		// Resources and state can only be retrieved from reachable members.
		entry.Resources, err = server.UseTarget(member.ServerName).GetServerResources()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed getting resources of member %q: %v\n", member.ServerName, err)
		}

		entry.State, _, err = server.GetClusterMemberState(member.ServerName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed getting state of member %q: %v\n", member.ServerName, err)
		}

		snap.Members = append(snap.Members, entry)
	}

	if len(snap.Members) == 0 {
		return errors.New("No cluster member found")
	}

	// Cluster groups.
	snap.Groups, err = server.GetClusterGroups()
	if err != nil {
		return fmt.Errorf("Failed getting cluster groups: %w", err)
	}

	// Projects.
	snap.Projects, err = server.GetProjects()
	if err != nil {
		return fmt.Errorf("Failed getting projects: %w", err)
	}

	// Profiles.
	snap.Profiles, err = server.GetProfilesAllProjects()
	if err != nil {
		return fmt.Errorf("Failed getting profiles: %w", err)
	}

	// Instances.
	snap.Instances, err = server.GetInstancesAllProjects(api.InstanceTypeAny)
	if err != nil {
		return fmt.Errorf("Failed getting instances: %w", err)
	}

	if !c.flagNoRedact {
		snap.redact()
	}

	err = snap.write(c.flagOutput)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Captured %d members, %d projects, %d profiles and %d instances\n", len(snap.Members), len(snap.Projects), len(snap.Profiles), len(snap.Instances))

	return nil
}
