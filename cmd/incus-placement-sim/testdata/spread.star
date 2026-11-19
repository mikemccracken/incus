# Example instance placement scriptlet.
#
# Instances sharing a "user.ha-group" value are kept on different cluster
# members, and the member with the most free memory is preferred.
def instance_placement(request, candidate_members):
    needed = get_instance_resources().memory_size
    group = request.config.get("user.ha-group", "")

    best = None
    best_free = -1
    for member in candidate_members:
        name = member.server_name

        if group != "":
            peers = [
                inst.name
                for inst in get_instances(project = request.project, location = name)
                if inst.name != request.name and inst.expanded_config.get("user.ha-group", "") == group
            ]

            if peers:
                log_info("skipping ", name, ": already hosts ", peers[0], " from group ", group)
                continue

        resources = get_cluster_member_resources(name)
        free = resources.memory.total - resources.memory.used
        if free < needed:
            log_warn("skipping ", name, ": not enough free memory")
            continue

        if free > best_free:
            best = name
            best_free = free

    if best == None:
        fail("No cluster member satisfies the placement rules")

    set_target(best)
