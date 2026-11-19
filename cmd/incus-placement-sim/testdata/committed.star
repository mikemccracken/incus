# Example instance placement scriptlet.
#
# Members are ranked by committed memory: the sum of the memory limits of the
# instances already placed on them. Unlike the live memory usage of a member,
# this total changes with every placement, so consecutive placements (an
# evacuation for example) get spread out rather than all landing on the member
# which had the most free memory when the first one was placed.

GIB = 1024 * 1024 * 1024

UNITS = {
    "": 1,
    "B": 1,
    "kB": 1000,
    "MB": 1000 * 1000,
    "GB": 1000 * 1000 * 1000,
    "TB": 1000 * 1000 * 1000 * 1000,
    "KiB": 1024,
    "MiB": 1024 * 1024,
    "GiB": GIB,
    "TiB": 1024 * GIB,
}

def instance_memory(inst, member_memory):
    """Returns the memory an instance is allowed to use, in bytes."""
    limit = inst.expanded_config.get("limits.memory", "")

    if limit == "":
        # Virtual machines default to 1GiB, containers have no limit.
        return GIB if inst.type == "virtual-machine" else 0

    if limit.endswith("%"):
        return member_memory * int(limit[:-1]) // 100

    number = limit.rstrip("BGKMTik")
    unit = limit[len(number):]
    if unit not in UNITS:
        log_warn("ignoring memory limit ", limit, " of instance ", inst.name)
        return 0

    return int(number) * UNITS[unit]

def instance_placement(request, candidate_members):
    needed = get_instance_resources().memory_size

    best = None
    best_free = -1
    for member in candidate_members:
        name = member.server_name
        total = get_cluster_member_resources(name).memory.total

        # Count the instances of all projects, leaving out the one being placed.
        committed = 0
        for inst in get_instances(location = name):
            if inst.project == request.project and inst.name == request.name:
                continue

            committed += instance_memory(inst, total)

        free = total - committed
        log_info(name, ": ", committed // GIB, "GiB committed, ", free // GIB, "GiB uncommitted")

        if free < needed:
            log_warn("skipping ", name, ": not enough uncommitted memory")
            continue

        if free > best_free:
            best = name
            best_free = free

    if best == None:
        fail("No cluster member has enough uncommitted memory")

    set_target(best)
