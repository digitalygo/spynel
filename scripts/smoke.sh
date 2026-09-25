#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
binary="$project_dir/.tmp-bin/spynel"

"$script_dir/dev.sh" dox
"$script_dir/dev.sh" build >/dev/null
smoke_dir=$(mktemp -d "${TMPDIR:-/tmp}/spynel-smoke.XXXXXX")
trap 'rm -rf "$smoke_dir"' EXIT HUP INT TERM

dev_bin_dir="$smoke_dir/user bin"
SPYNEL_DEV_BIN_DIR="$dev_bin_dir" "$script_dir/install-dev.sh" >/dev/null
test -x "$dev_bin_dir/spynel"
"$dev_bin_dir/spynel" version >/dev/null

# `docs` is workspace-independent and must expose exactly the retained topic
# set, with no retired task/goal workflow topics.
docs_index=$(cd "$smoke_dir" && "$binary" docs)
expected_docs_topics=$(printf '%s\n' architecture channels commands configuration harnesses instances-primary integration jobs logs notifications security troubleshooting workspace-state | sort)
docs_topics=$(printf '%s\n' "$docs_index" | sed -n 's/^- `\([^`]*\)`.*/\1/p' | sort)
if [ "$docs_topics" != "$expected_docs_topics" ]; then
  echo "documentation index does not match the exact retained topic set:" >&2
  printf '%s\n' "$docs_topics" >&2
  exit 1
fi
docs_json=$(cd "$smoke_dir" && "$binary" docs search session --format json)
printf '%s\n' "$docs_json" | grep -q '"schema_version": "spynel.docs/v1"'

# CLI help advertises the retained job controls only, with no workflow copy.
help_output=$("$binary" help)
printf '%s\n' "$help_output" | grep -q 'spynel job info N'
printf '%s\n' "$help_output" | grep -q 'spynel job output N'
printf '%s\n' "$help_output" | grep -q 'spynel job kill N'
for copy in 'job message' 'job ping' 'spynel task' 'spynel goal' 'spynel instructions' 'spynel run' 'orchestration'; do
  if printf '%s\n' "$help_output" | grep -q "$copy"; then
    echo "retired or stale copy \"$copy\" remains in CLI help" >&2
    exit 1
  fi
done

"$binary" init --no-start --dir "$smoke_dir"
(cd "$smoke_dir" && "$binary" config)

# Fresh initialization creates the current workspace contract only; retired
# task, goal, prompt, and instruction workflow assets are never recreated.
test -f "$smoke_dir/.spynel/config.yaml"
test ! -e "$smoke_dir/spynel.yaml"
if grep -q 'state_dir:' "$smoke_dir/.spynel/config.yaml"; then
  echo "canonical config unexpectedly contains workspace.state_dir" >&2
  exit 1
fi
if grep -Eq '^[[:space:]]*(reviews|chat_agent_prefix|developer_agent_prefix|reviewer_agent_prefix|heartbeat_agent_prefix):' "$smoke_dir/.spynel/config.yaml"; then
  echo "canonical config unexpectedly retains retired workflow keys" >&2
  exit 1
fi
test -f "$smoke_dir/.spynel/AGENTS.md"
test ! -d "$smoke_dir/.spynel/prompts"
test ! -d "$smoke_dir/.spynel/tasks"
test ! -d "$smoke_dir/.spynel/goals"
if [ -d "$smoke_dir/.spynel/instructions" ]; then
  test "$(find "$smoke_dir/.spynel/instructions" -type f | wc -l)" -eq 0
fi
for retired in instructions tasks goals task goal; do
  if (cd "$smoke_dir" && "$binary" "$retired") >/dev/null 2>&1; then
    echo "retired workflow command $retired unexpectedly succeeded" >&2
    exit 1
  fi
done

status_json=$("$binary" status --config "$smoke_dir/.spynel/config.yaml" --conversation smoke --json)
printf '%s\n' "$status_json" | grep -q '"harness_state"'
printf '%s\n' "$status_json" | grep -q '"connections"'

command_output=$("$binary" command --config "$smoke_dir/.spynel/config.yaml" --conversation smoke help commands)
printf '%s\n' "$command_output" | grep -q '/status'
printf '%s\n' "$command_output" | grep -q '/jobs'
printf '%s\n' "$command_output" | grep -q '/job info'
printf '%s\n' "$command_output" | grep -q '/job output'
printf '%s\n' "$command_output" | grep -q '/job kill'
for copy in '/tasks' '/goals'; do
  if printf '%s\n' "$command_output" | grep -q "$copy"; then
    echo "retired workflow slash command $copy remains in the catalog" >&2
    exit 1
  fi
done

conversation_json=$("$binary" conversations list --config "$smoke_dir/.spynel/config.yaml" --json)
printf '%s\n' "$conversation_json" | grep -q '"conversation":"smoke"'
"$binary" conversations show --config "$smoke_dir/.spynel/config.yaml" --tail 5 cli smoke >/dev/null
branch=$("$binary" conversations resume --config "$smoke_dir/.spynel/config.yaml" cli smoke)
case "$branch" in
  resume-????????) ;;
  *) echo "unexpected resumed CLI conversation: $branch" >&2; exit 1 ;;
esac
"$binary" conversations show --config "$smoke_dir/.spynel/config.yaml" --tail 5 cli "$branch" >/dev/null

if "$binary" followup --config "$smoke_dir/.spynel/config.yaml" --conversation smoke "too late" >/dev/null 2>&1; then
  echo "inactive plain CLI follow-up unexpectedly succeeded" >&2
  exit 1
fi

# Existing retired workflow documents survive an upgrade byte-for-byte and
# never surface through retained commands.
upgrade_dir="$smoke_dir/upgrade"
"$binary" init --no-start --dir "$upgrade_dir" >/dev/null
mkdir -p "$upgrade_dir/.spynel/tasks/todo" "$upgrade_dir/.spynel/goals/proposed" "$upgrade_dir/.spynel/prompts" "$upgrade_dir/.spynel/instructions"
sentinel='RETIRED WORKFLOW SENTINEL'
for legacy in tasks/todo/legacy-task.md goals/proposed/legacy-goal.md prompts/chat.md instructions/agent-chat.md; do
  printf '%s\n' "$sentinel" > "$upgrade_dir/.spynel/$legacy"
done
legacy_before=$(cd "$upgrade_dir/.spynel" && cksum tasks/todo/legacy-task.md goals/proposed/legacy-goal.md prompts/chat.md instructions/agent-chat.md)
upgrade_status=$("$binary" status --config "$upgrade_dir/.spynel/config.yaml" --json)
legacy_after=$(cd "$upgrade_dir/.spynel" && cksum tasks/todo/legacy-task.md goals/proposed/legacy-goal.md prompts/chat.md instructions/agent-chat.md)
if [ "$legacy_before" != "$legacy_after" ]; then
  echo "upgrade modified retired workflow documents" >&2
  exit 1
fi
if printf '%s\n' "$upgrade_status" | grep -q "$sentinel"; then
  echo "retired workflow documents leaked into retained command output" >&2
  exit 1
fi
test ! -e "$upgrade_dir/.spynel/prompts/create-task.md"
test ! -e "$upgrade_dir/.spynel/prompts/create-goal.md"
test ! -e "$upgrade_dir/.spynel/prompts/goal-review.md"
test "$(find "$upgrade_dir/.spynel/tasks" -type f | wc -l)" -eq 1
test "$(find "$upgrade_dir/.spynel/goals" -type f | wc -l)" -eq 1
test "$(find "$upgrade_dir/.spynel/instructions" -type f | wc -l)" -eq 1
(cd "$upgrade_dir" && "$binary" config) >/dev/null

echo "Spynel smoke test passed: $smoke_dir"
