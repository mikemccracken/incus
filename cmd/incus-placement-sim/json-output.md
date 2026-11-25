# JSON output of incus-placement-sim

`incus-placement-sim run` and `incus-placement-sim test` print a JSON document
when passed `--format json`. The document describes everything the simulator
did, in enough detail for another program to replay it one placement at a time.

This is the reference for schema version 1.

## Running the simulator

```bash
incus-placement-sim run evacuate node2 -c cluster.yaml -s placement.star --compare --format json
incus-placement-sim run create -c cluster.yaml -s placement.star --name web9 --format json
incus-placement-sim run move web1 --target @default -c cluster.yaml --format json
incus-placement-sim test scenario.star --format json
```

| Stream or status | Content |
| :--------------- | :------ |
| stdout           | The JSON document and nothing else, indented, ending with a newline. |
| stderr           | Notices and errors meant for people. Never parse it. |
| exit status 0    | The simulation ran. For `test`, every test passed. |
| exit status 1    | Either tests failed or the simulator couldn't run. |

Tell the two failures apart by looking at stdout:

- When tests fail, stdout still holds the complete document and each failed
  run has `test.passed` set to `false`.
- When the simulator can't run (unreadable file, invalid scriptlet, unknown
  instance or cluster member passed to `run`), stdout is empty and stderr has
  the error.

A simulation takes a few milliseconds, so there is no streaming. Read all of
stdout, then parse it.

## Structure

```
document
└── runs[]                  one timeline each
    ├── cluster             what doesn't change: members, their capacity, groups
    ├── initial             cluster state before the first action
    ├── actions[]           the requests which were simulated
    │   └── placements[]    one per instance the action had to place
    ├── final               cluster state after the last action
    └── test                verdict, for the "test" command only
```

Every run starts again from the cluster as it was captured. Runs never affect
each other and each one carries all it needs to be shown on its own.

The types below are written as TypeScript declarations. A field marked with
`?` is left out when it has no value. Lists are never `null` and are empty
rather than missing. The only field which can be `null` is `change`, along with
the `resources` of a cluster member.

## Document

```ts
interface Document {
  schema_version: number; // 1
  command: "run" | "test";
  runs: Run[];
}
```

What a run stands for depends on the command:

| Command | Runs |
| :------ | :--- |
| `run`   | One per placement logic, all simulating the same action. A single run unless `--compare` or several `-s` are used. With a comparison, the first run is the built-in placement. |
| `test`  | One per `test_` function, in the order of the scenario files on the command line, then in the order of the functions within a file. |

## Run

```ts
interface Run {
  name: string;
  scenario?: string;      // "test" only
  scriptlet: string;
  has_scriptlet: boolean;
  cluster: Cluster;
  initial: State;
  actions: Action[];
  final: State;
  test?: Test;            // "test" only
}
```

| Field           | `run`                                                                   | `test` |
| :-------------- | :---------------------------------------------------------------------- | :----- |
| `name`          | `built-in`, `captured` or the file name of a scriptlet passed with `-s` | Name of the test function |
| `scenario`      | Absent                                                                  | Path of the scenario file, as passed on the command line |
| `scriptlet`     | Same as `name`                                                          | `built-in placement`, `captured scriptlet` or the path of the scriptlet file |
| `has_scriptlet` | `false` when the built-in placement is all there is                     | Same |

`captured` is the scriptlet which was set on the cluster
(`instances.placement.scriptlet`) when the snapshot was taken.

`name` is unique within a `run` document. In a `test` document, use
`scenario` together with `name` as the key.

## Cluster

The part of the cluster which stays the same during a run.

```ts
interface Cluster {
  captured_at: string;    // RFC 3339 time of the snapshot
  server_version: string;
  members: Member[];      // in snapshot order
  groups: Group[];
}

interface Member {
  name: string;
  architecture: string;   // "x86_64", "aarch64", ...
  groups: string[];
  roles: string[];
  failure_domain: string;
  scheduler: string;      // scheduler.instance: "", "all", "manual" or "group"
  resources: {
    cpus: number;         // CPU threads
    memory: number;       // bytes
    memory_used: number;  // bytes, at capture time
  } | null;               // null if the member couldn't be queried during the capture
}

interface Group {
  name: string;
  members: string[];
}
```

`members` is the list to draw the cluster from. Its order is stable, which
makes it a good default layout order. The status of a member isn't here as it
changes during a run, see `State`.

`resources` describes the hardware and what was in use when the snapshot was
taken. It never changes during a run, the simulator doesn't model real usage.
What changes is what instances are allowed to use, see `committed_cpus` and
`committed_memory` below.

## State

The part of the cluster which changes.

```ts
interface State {
  members: { [name: string]: MemberState };
  instances: Instance[];
}

interface MemberState {
  status: string;           // "Online", "Evacuated", "Offline", ...
  instances: number;        // instances on the member, all projects
  committed_cpus: number;   // sum of the CPU limits of those instances
  committed_memory: number; // sum of their memory limits, in bytes
}

interface Instance {
  project: string;
  name: string;
  type: "container" | "virtual-machine";
  location: string;         // cluster member
  status: string;           // as captured: "Running", "Stopped", ...
  cpus: number;
  memory: number;           // bytes
  root_disk: number;        // bytes
}
```

`members` has an entry for every member of `cluster.members`.

Committed resources add up the limits (`limits.cpu`, `limits.memory`) of the
instances on a member, using the same rules as the daemon. An instance without
a limit counts as 0, except for virtual machines which default to 1 CPU and
1GiB of memory. A value of 0 for `cpus`, `memory` or `root_disk` therefore
means "no limit set", not "nothing used".

An instance is identified by `project` and `name` together. The `status` of an
instance is the one from the snapshot and isn't updated by the simulation, even
for instances an evacuation would stop.

`initial.instances` is in snapshot order. `final.instances` has the same
instances with their new `location`, followed by the instances which were
created, in creation order.

## Action

Something the simulator was asked to do.

```ts
interface Action {
  index: number;          // position in the run, from 0
  type: "create" | "move" | "evacuate";
  params: { [name: string]: any };
  placements: Placement[];
  error?: string;
}
```

| Type       | `params`                                                                                           | Placements |
| :--------- | :------------------------------------------------------------------------------------------------- | :--------- |
| `create`   | `project`, `name`, `type`, and when set: `target`, `architecture`, `image`, `profiles`, `config`, `devices` | One |
| `move`     | `project`, `instance`, `target`                                                                    | One |
| `evacuate` | `member`, `heal`                                                                                   | One per instance on the member, in snapshot order. None if the member is empty. |

A `target` is either the name of a cluster member or `@` followed by the name
of a cluster group.

With `heal` set, the member is treated as having gone offline (automatic
healing) rather than being evacuated by an administrator. Its status becomes
`Offline` instead of `Evacuated`.

`error` is only set when the action couldn't be simulated at all, for example
because the instance doesn't exist. Such an action has no placements and
changes nothing. This only shows up in `test` documents, where it also fails
the test. It is different from a placement being rejected, which is a normal
result with its own `error`.

## Placement

The placement of one instance. This is the unit to step through.

```ts
interface Placement {
  // Identity
  index: number;                      // position in the run, from 0, across all actions
  reason: "new" | "relocation" | "evacuation";
  project: string;
  instance: string;
  instance_type: "container" | "virtual-machine";
  resources: { cpus: number; memory: number; root_disk: number };
  source?: string;                    // member the instance is on, absent for a creation

  // Evaluation
  candidates: Candidate[];
  excluded: Exclusion[];
  trace: TraceEntry[];
  duration_us: number;                // time spent in the scriptlet, 0 if it wasn't called

  // Decision
  outcome: "scriptlet" | "builtin" | "validated" | "rejected" | "stays";
  target?: string;                    // member the instance ends up on
  detail?: string;                    // explanation of the outcome, for people
  error?: string;                     // why the placement was rejected

  // Effect
  change: Change | null;
  members: { [name: string]: MemberState };
}
```

`reason` is the value the scriptlet receives in `request.reason`. It follows
from the action: `new` for `create`, `relocation` for `move` and `evacuation`
for `evacuate`.

### Evaluation

```ts
interface Candidate {
  member: string;
  instances: number;  // instances on the member when the placement started
}

interface Exclusion {
  member: string;
  code: "status" | "scheduler_manual" | "scheduler_group"
      | "cluster_group" | "project_cluster_groups" | "architecture";
  reason: string;     // the same, for people
}
```

`candidates` are the members handed to the scriptlet as `candidate_members`,
in the order it got them: fewest instances first. The built-in placement takes
the first one.

`excluded` are the members which were filtered out before the scriptlet ran.

| Code                     | The member was left out because |
| :----------------------- | :------------------------------ |
| `status`                 | It isn't online (evacuated, offline, ...). |
| `scheduler_manual`       | It has `scheduler.instance=manual` and only takes instances which target it by name. |
| `scheduler_group`        | It has `scheduler.instance=group` and no cluster group was targeted. |
| `cluster_group`          | It isn't part of the targeted cluster group. |
| `project_cluster_groups` | It isn't part of a cluster group the project is restricted to. |
| `architecture`           | It can't run the architecture of the instance. |

When a request targets a member by name, that member is the only candidate
and `excluded` is empty: the other members were never considered. So don't
assume that `candidates` and `excluded` together cover the whole cluster.

Both lists are empty for an instance which isn't migrated by an evacuation
(`outcome` is `stays` and `detail` says why).

### Trace

What the scriptlet did, in order. Empty when no scriptlet ran.

```ts
interface TraceEntry {
  kind: "call" | "log";
  message: string;                    // ready to display

  // kind "call"
  function?: string;
  args?: { [name: string]: string | boolean };

  // kind "log"
  level?: "info" | "warn" | "error";
  fields?: { [name: string]: string };

  // either kind
  member?: string;                    // the cluster member the entry is about
}
```

A `call` is a function the scriptlet called to look at the cluster or to make
its choice:

| Function                       | `args`                           | `member` |
| :----------------------------- | :------------------------------- | :------- |
| `get_instance_resources`       | None, `args` is absent           | Absent |
| `get_cluster_members`          | `group`                          | Absent |
| `get_cluster_member_resources` | `member_name`                    | The member |
| `get_cluster_member_state`     | `member_name`                    | The member |
| `get_instances`                | `project`, `location`            | The location, absent when it is empty |
| `get_instances_count`          | `project`, `location`, `pending` | The location, absent when it is empty |
| `get_project`                  | `name`                           | Absent |
| `set_target`                   | `member_name`                    | The member which was picked |

An empty string for `project`, `location` or `group` means the scriptlet didn't
filter on it.

A `log` is normally a message from `log_info`, `log_warn` or `log_error` in
the scriptlet. Those have no `fields` and no `member`: a scriptlet which
wants to explain itself can only do so in the text of the message.

The one log entry with `fields` comes from the placement engine. When a
scriptlet calls `set_target` with a member which isn't a candidate, the trace
gets an `error` entry with the name in `fields.member` and in `member`,
instead of a `set_target` call. The placement is then rejected.

`member` is what to highlight while showing an entry: the scriptlet is looking
at that member, or picking it.

A scriptlet can call `set_target` more than once, the last call wins. It can
also end with `fail()`, in which case the trace stops there, the placement is
rejected and `error` carries the message.

### Decision

| Outcome     | Meaning | `target` | `change` |
| :---------- | :------ | :------- | :------- |
| `scriptlet` | The scriptlet picked the member with `set_target`. | Set | Set |
| `builtin`   | No scriptlet, or it returned without picking: the candidate with the fewest instances was used. `detail` says which of the two. | Set | Set |
| `validated` | A move targeted a member by name and the scriptlet let it through. A scriptlet can't redirect such a move, only reject it. | Set | Set, or `null` if the instance was already there |
| `rejected`  | The request fails: the scriptlet failed, or there is no candidate. `error` has the message a user would get. | Absent | `null` |
| `stays`     | Evacuation only. The instance isn't moved, either because of its `cluster.evacuate` setting or because there is nowhere to go. | Same as `source` | `null` |

`validated` is only used for moves. A creation which targets a member by name
is reported as `scriptlet` or `builtin`, with that member as the only
candidate.

`detail` and `error` are English sentences for display. Don't match on them,
use `outcome` and the exclusion codes.

### Effect

```ts
interface Change {
  type: "create" | "move";
  from?: string;  // absent for "create"
  to: string;
}
```

`change` is the movement to animate: an instance appearing on `to`, or going
from `from` to `to`. It is `null` when nothing moved.

`members` is the state of every cluster member right after this placement,
in the same form as `initial.members`. It is there even when `change` is
`null`.

During an evacuation, the status of the member being emptied changes before
its first instance is placed, so the first placement of the action already
shows it as `Evacuated` or `Offline`.

## Test

```ts
interface Test {
  passed: boolean;
  failure?: string;   // "file:line: message", only when the test failed
  output: string[];   // lines printed by the test with print()
}
```

A test stops at its first failure. The actions which ran before that point are
in the run, with their placements.

Checks (`assert_eq`, `assert_true`) and the queries a test makes between
actions aren't part of the timeline. Only the verdict is recorded.

## Replaying a run

The steps of a run are its placements, in order:

```ts
const steps = run.actions.flatMap((action) =>
  action.placements.map((placement) => ({ action, placement })));
```

`placement.index` is the position in that list.

**Where instances are.** Start from `run.initial.instances`. Going forward,
apply the `change` of each placement:

```ts
function apply(instances: Map<string, Instance>, p: Placement) {
  if (p.change === null) {
    return;
  }

  const key = p.project + "/" + p.instance;
  if (p.change.type === "create") {
    instances.set(key, {
      project: p.project,
      name: p.instance,
      type: p.instance_type,
      location: p.change.to,
      status: "Stopped",
      ...p.resources,
    });
  } else {
    instances.get(key)!.location = p.change.to;
  }
}
```

To go backwards, undo the change (delete the created instance, or set the
location back to `from`), or rebuild from `initial` up to the wanted step.
After the last step, the result matches `run.final.instances`.

**Member totals.** No need to compute them. At step `i`, use
`steps[i].placement.members`; before the first step, use `run.initial.members`.

**A suggested sequence for one step:**

1. Show the request: `reason`, the instance, its `resources` and its `source`.
2. Dim the `excluded` members, with `reason` as the explanation. Mark the
   `candidates` and show their instance count.
3. Play the `trace` one entry at a time, highlighting `member` when set.
4. Show the decision: `outcome`, `target`, and `detail` or `error`.
5. Animate `change`, then update the members from `members`.

**Comparing placement logics.** In a `run` document with several runs, every
run simulates the same request from the same starting point, so they can be
shown side by side. For `create` and `move` each run has one placement. For
`evacuate`, the runs handle the same instances in the same order, so
placements can be paired up by `index`.

## Example

A database instance being moved off `node2` by an evacuation, with a scriptlet
which keeps databases on separate members. Some trace entries and members are
left out.

```json
{
  "index": 0,
  "reason": "evacuation",
  "project": "default",
  "instance": "db2",
  "instance_type": "virtual-machine",
  "resources": { "cpus": 4, "memory": 8589934592, "root_disk": 53687091200 },
  "source": "node2",
  "candidates": [
    { "member": "node3", "instances": 1 },
    { "member": "node1", "instances": 3 }
  ],
  "excluded": [
    { "member": "node2", "code": "status", "reason": "status is Evacuated" },
    { "member": "node4", "code": "scheduler_group", "reason": "scheduler.instance=group and no cluster group was targeted" }
  ],
  "trace": [
    {
      "kind": "call",
      "message": "get_instances(project=\"default\", location=\"node1\")",
      "function": "get_instances",
      "args": { "location": "node1", "project": "default" },
      "member": "node1"
    },
    {
      "kind": "log",
      "level": "info",
      "message": "skipping node1: already hosts db1 from group db"
    },
    {
      "kind": "call",
      "message": "set_target(member_name=\"node3\")",
      "function": "set_target",
      "args": { "member_name": "node3" },
      "member": "node3"
    }
  ],
  "outcome": "scriptlet",
  "target": "node3",
  "duration_us": 157,
  "change": { "type": "move", "from": "node2", "to": "node3" },
  "members": {
    "node2": { "status": "Evacuated", "instances": 1, "committed_cpus": 2, "committed_memory": 2147483648 },
    "node3": { "status": "Online", "instances": 2, "committed_cpus": 6, "committed_memory": 10737418240 }
  }
}
```

Full documents can be produced from the files in `testdata`:

```bash
cd cmd/incus-placement-sim/testdata
incus-placement-sim run evacuate node2 -c cluster.yaml -s spread.star --compare --format json
incus-placement-sim test scenario.star --format json
```

## What the simulation leaves out

- An evacuation is simulated one instance at a time, each move being applied
  before the next instance is placed. A real evacuation works on several
  instances at once.
- Member resources (`cluster.members[].resources`) are the ones from the
  snapshot. A scriptlet asking for them gets the same answer throughout a run.
- Instances being created at the same moment (what `get_instances_count` calls
  pending) don't exist in the simulation.
- `duration_us` is how long the scriptlet took on the machine running the
  simulator. It varies between runs of the same simulation; everything else in
  the document is the same every time.

## Versioning

`schema_version` goes up when a change would break a reader: a field removed
or renamed, or its meaning changed. Check it and refuse versions you don't
know.

Fields and values can be added without the version changing. Ignore fields you
don't know, and handle unknown values of `outcome`, exclusion `code`, action
`type`, trace `kind` and `function` without failing.
