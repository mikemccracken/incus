#!/bin/sh
# Builds a placement snapshot using only "incus query", which defaults to GET.
# Usage: capture-readonly.sh [<remote>:] > cluster.yaml
set -eu

R="${1:-}"
D="$(mktemp -d "${TMPDIR:-/tmp}/placement.XXXXXX")"
trap 'rm -rf "$D"' EXIT

get() { incus query "${R}$1"; }

get "/1.0" > "$D/server.json"
get "/1.0/cluster/members?recursion=1" > "$D/members.json"
get "/1.0/cluster/groups?recursion=1" > "$D/groups.json"
get "/1.0/projects?recursion=1" > "$D/projects.json"
get "/1.0/profiles?recursion=1&all-projects=true" > "$D/profiles.json"
get "/1.0/instances?recursion=1&all-projects=true" > "$D/instances.json"

echo '{}' > "$D/extra.json"
for m in $(jq -r '.[].server_name' "$D/members.json"); do
    get "/1.0/resources?target=$m" > "$D/res.json" || echo null > "$D/res.json"
    get "/1.0/cluster/members/$m/state" > "$D/state.json" || echo null > "$D/state.json"
    jq --arg m "$m" --slurpfile r "$D/res.json" --slurpfile s "$D/state.json" \
        '. + {($m): {resources: $r[0], state: $s[0]}}' "$D/extra.json" > "$D/extra.new"
    mv "$D/extra.new" "$D/extra.json"
done

jq -n \
    --slurpfile server "$D/server.json" --slurpfile members "$D/members.json" \
    --slurpfile extra "$D/extra.json" --slurpfile groups "$D/groups.json" \
    --slurpfile projects "$D/projects.json" --slurpfile profiles "$D/profiles.json" \
    --slurpfile instances "$D/instances.json" '
  def redact: if . == null then . else with_entries(
    if (.key | test("^(cloud-init|environment)\\.|^user\\.(user-data|vendor-data|network-config|meta-data)$"))
    then .value = "<redacted>" else . end) end;
  {
    version: 1,
    captured_at: (now | todate),
    server: {
      version: $server[0].environment.server_version,
      config: (($server[0].config // {}) | with_entries(select(.key | test(
        "^(cluster\\.(healing_threshold|offline_threshold|rebalance\\..*)|images\\.default_architecture|instances\\.placement\\.scriptlet)$"))))
    },
    members: [$members[0][] | . + ($extra[0][.server_name] // {})],
    groups: $groups[0],
    projects: $projects[0],
    profiles: [$profiles[0][] | .config |= redact],
    instances: [$instances[0][] | .config |= redact | .expanded_config |= redact]
  }'
