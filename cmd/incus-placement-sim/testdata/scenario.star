# Example scenario for the "test" command.
#
# Database instances share a "user.ha-group" value, which spread.star keeps on
# different cluster members.
cluster = "cluster.yaml"
scriptlet = "spread.star"

def assert_apart(names):
    """Checks that the named instances are all on different cluster members."""
    hosts = {}
    for inst in get_instances(project = "default"):
        if inst.name in names:
            assert_true(inst.location not in hosts, inst.name + " shares " + inst.location + " with " + hosts.get(inst.location, ""))
            hosts[inst.location] = inst.name

def assert_no_member_above(limit):
    """Checks that no cluster member accepting instances has more than limit of them."""
    for member in get_cluster_members():
        count = get_instances_count(location = member.server_name)
        assert_true(count <= limit, "%s has %d instances, expected at most %d" % (member.server_name, count, limit))

def test_databases_stay_apart_through_evacuation():
    placed = evacuate("node2")

    assert_eq(placed["db2"].outcome, "scriptlet")
    assert_eq(placed["db2"].target, "node3")
    assert_eq(placed["cache1"].outcome, "stays")
    assert_eq(get_instances_count(location = "node2"), 1)
    assert_no_member_above(3)
    assert_apart(["db1", "db2"])

    # A third database instance has nowhere to go...
    db3 = create(name = "db3", type = "virtual-machine", profiles = ["default", "db"])
    assert_eq(db3.outcome, "rejected")
    assert_true("No cluster member satisfies the placement rules" in db3.error)

    # ...unless the gpu group is requested.
    db3 = create(name = "db3", type = "virtual-machine", profiles = ["default", "db"], target = "@gpu")
    assert_eq(db3.outcome, "scriptlet")
    assert_eq(db3.target, "node4")
    assert_apart(["db1", "db2", "db3"])

def test_database_cannot_join_another():
    assert_eq(move("db1", target = "node2").outcome, "rejected")
    assert_eq(move("db1", target = "node3").outcome, "validated")

def test_other_instances_move_freely():
    moved = move("web1", target = "@default")
    assert_true(moved.target in ["node2", "node3"], "unexpected target " + moved.target)
    assert_no_member_above(3)
