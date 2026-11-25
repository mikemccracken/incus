package main

import (
	"encoding/json"
	"io"
	"time"
)

// documentVersion is bumped whenever the JSON output changes in a way readers have to adapt to.
const documentVersion = 1

// document is the JSON output, made to be replayed step by step by another program.
type document struct {
	SchemaVersion int      `json:"schema_version"`
	Command       string   `json:"command"`
	Runs          []docRun `json:"runs"`
}

// docRun is one timeline, starting from the cluster as captured: a placement logic for "run", a test for "test".
type docRun struct {
	Name         string       `json:"name"`
	Scenario     string       `json:"scenario,omitempty"`
	Scriptlet    string       `json:"scriptlet"`
	HasScriptlet bool         `json:"has_scriptlet"`
	Cluster      docCluster   `json:"cluster"`
	Initial      clusterState `json:"initial"`
	Actions      []docAction  `json:"actions"`
	Final        clusterState `json:"final"`
	Test         *docTest     `json:"test,omitempty"`
}

// docCluster is the part of the cluster which doesn't change during a run.
type docCluster struct {
	CapturedAt    time.Time   `json:"captured_at"`
	ServerVersion string      `json:"server_version"`
	Members       []docMember `json:"members"`
	Groups        []docGroup  `json:"groups"`
}

// docMember describes a cluster member, its status being part of the cluster state.
type docMember struct {
	Name          string        `json:"name"`
	Architecture  string        `json:"architecture"`
	Groups        []string      `json:"groups"`
	Roles         []string      `json:"roles"`
	FailureDomain string        `json:"failure_domain"`
	Scheduler     string        `json:"scheduler"`
	Resources     *docResources `json:"resources"`
}

// docResources is the capacity of a cluster member, missing if it couldn't be reached during the capture.
type docResources struct {
	CPUs       uint64 `json:"cpus"`
	Memory     uint64 `json:"memory"`
	MemoryUsed uint64 `json:"memory_used"`
}

// docGroup is a cluster group.
type docGroup struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// docAction is a simulated request and the placements it led to, in the order they happened.
type docAction struct {
	Index      int            `json:"index"`
	Type       string         `json:"type"`
	Params     map[string]any `json:"params"`
	Placements []docPlacement `json:"placements"`
	Error      string         `json:"error,omitempty"`
}

// docPlacement is the placement of one instance: what was considered, what was decided and what changed.
type docPlacement struct {
	Index        int                    `json:"index"`
	Reason       string                 `json:"reason"`
	Project      string                 `json:"project"`
	Instance     string                 `json:"instance"`
	InstanceType string                 `json:"instance_type"`
	Resources    instanceUsage          `json:"resources"`
	Source       string                 `json:"source,omitempty"`
	Candidates   []docCandidate         `json:"candidates"`
	Excluded     []exclusion            `json:"excluded"`
	Trace        []traceEntry           `json:"trace"`
	Outcome      string                 `json:"outcome"`
	Target       string                 `json:"target,omitempty"`
	Detail       string                 `json:"detail,omitempty"`
	Error        string                 `json:"error,omitempty"`
	DurationUS   int64                  `json:"duration_us"`
	Change       *docChange             `json:"change"`
	Members      map[string]memberState `json:"members"`
}

// docCandidate is a cluster member the instance could go to, in the order the scriptlet got them.
type docCandidate struct {
	Member    string `json:"member"`
	Instances int    `json:"instances"`
}

// docChange is the movement a placement caused, a creation having no origin.
type docChange struct {
	Type string `json:"type"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

// docTest is the verdict of a scenario test.
type docTest struct {
	Passed  bool     `json:"passed"`
	Failure string   `json:"failure,omitempty"`
	Output  []string `json:"output"`
}

// docPlacementOf converts a placement result.
func docPlacementOf(index int, res *result) docPlacement {
	p := docPlacement{
		Index:        index,
		Reason:       res.Reason,
		Project:      res.Project,
		Instance:     res.Instance,
		InstanceType: res.InstanceType,
		Resources:    res.Resources,
		Source:       res.Source,
		Candidates:   make([]docCandidate, 0, len(res.Candidates)),
		Excluded:     res.Excluded,
		Trace:        res.Trace,
		Outcome:      res.Outcome,
		Target:       res.Target,
		Detail:       res.Detail,
		Error:        res.Error,
		DurationUS:   res.DurationMicros,
		Members:      res.MembersAfter,
	}

	for _, name := range res.Candidates {
		p.Candidates = append(p.Candidates, docCandidate{Member: name, Instances: res.InstanceCounts[name]})
	}

	// Empty lists are easier on readers than missing ones.
	if p.Excluded == nil {
		p.Excluded = []exclusion{}
	}

	if p.Trace == nil {
		p.Trace = []traceEntry{}
	}

	// Rejected instances and those staying in place don't go anywhere.
	if res.Outcome == outcomeRejected || res.Outcome == outcomeStays || res.Target == res.Source {
		return p
	}

	p.Change = &docChange{Type: "move", From: res.Source, To: res.Target}
	if res.Source == "" {
		p.Change.Type = "create"
	}

	return p
}

// document returns everything that happened to the simulator as a run.
func (s *simulator) document(name string, scriptletName string) docRun {
	run := docRun{
		Name:         name,
		Scriptlet:    scriptletName,
		HasScriptlet: s.hasScriptlet,
		Cluster: docCluster{
			CapturedAt:    s.snap.CapturedAt,
			ServerVersion: s.snap.Server.Version,
			Members:       make([]docMember, 0, len(s.snap.Members)),
			Groups:        make([]docGroup, 0, len(s.snap.Groups)),
		},
		Initial: s.initial,
		Actions: make([]docAction, 0, len(s.actions)),
		Final:   s.state(),
	}

	for _, member := range s.snap.Members {
		entry := docMember{
			Name:          member.ServerName,
			Architecture:  member.Architecture,
			Groups:        append([]string{}, member.Groups...),
			Roles:         append([]string{}, member.Roles...),
			FailureDomain: member.FailureDomain,
			Scheduler:     member.Config["scheduler.instance"],
		}

		if member.Resources != nil {
			entry.Resources = &docResources{
				CPUs:       member.Resources.CPU.Total,
				Memory:     member.Resources.Memory.Total,
				MemoryUsed: member.Resources.Memory.Used,
			}
		}

		run.Cluster.Members = append(run.Cluster.Members, entry)
	}

	for _, group := range s.snap.Groups {
		run.Cluster.Groups = append(run.Cluster.Groups, docGroup{Name: group.Name, Members: append([]string{}, group.Members...)})
	}

	// Placements are numbered across the whole run so that they can be stepped through.
	placements := 0
	for i, act := range s.actions {
		entry := docAction{Index: i, Type: act.Type, Params: act.Params, Placements: make([]docPlacement, 0, len(act.Results)), Error: act.Error}
		for _, res := range act.Results {
			entry.Placements = append(entry.Placements, docPlacementOf(placements, res))
			placements++
		}

		run.Actions = append(run.Actions, entry)
	}

	return run
}

// writeDocument writes the JSON output.
func writeDocument(w io.Writer, command string, runs []docRun) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(document{SchemaVersion: documentVersion, Command: command, Runs: runs})
}
