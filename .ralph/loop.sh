#!/usr/bin/env bash
# ralph-implement — autonomous pipeline: design doc → stack → plan → build → review → PR
#
# Usage:
#   .ralph/loop.sh <design-doc> [options]
#   .ralph/loop.sh --resume [options]
#
# Options:
#   --resume             Continue from .ralph/state
#   --from PHASE         Force start phase: stack|plan|build|review|consolidate|pr
#   --review-every N     Run a review pass every N build iterations (default 5; 0 = only at the end)
#   --no-pr              Stop after review; do not open a PR
#   --no-tracker         Do not require Loop Tracker
#   --plan-max N         Max plan iterations (default 5)
#   --build-max N        Max build iterations (default: 2x task count, min 10)
#   --review-max N       Max review cycles (default 3)
#   --branch-cap N       Insertions against the base branch before the run stops to be
#                        merged (default 4000; 0 disables). A branch nobody can review
#                        is not reviewed.
#   --time-budget SECS   Wall-clock budget (default: none — the phase runs until its
#                        iteration cap or completion, however long that takes)
#
# Exit codes: 0 shipped/complete, 1 error, 2 stopped at a cap (resumable)

set -uo pipefail   # deliberately NOT -e: phase exit codes are control flow

RALPH_DIR=".ralph"
STATE_FILE="$RALPH_DIR/state"
STACK_FILE="$RALPH_DIR/stack.json"

DESIGN_DOC=""
RESUME=false
FORCE_PHASE=""
DO_PR=true
NO_TRACKER=false
PLAN_MAX=5
BUILD_MAX=0            # 0 = derive from task count
REVIEW_MAX=3
REVIEW_EVERY=6        # review every N build iterations; 0 = only once, after all tasks.
                      # 6 = the planner's block size (5 feature tasks + 1 refactor pass), so
                      # each checkpoint lands just after a refactor pass rather than before it.
TIME_BUDGET=99999999  # ~3 years: the work is bounded by the task list, not by the clock.
                      # The iteration caps (BUILD_MAX, REVIEW_MAX) are the real stops; a
                      # wall-clock stop only ever interrupted a phase mid-way.
RETRY_MAX=5
RETRY_DELAY=30
BRANCH_CAP=${BRANCH_CAP:-4000}  # insertions against the base branch before the run stops
                                # to be merged. Override with --branch-cap or the env var.

while [ $# -gt 0 ]; do
  case "$1" in
    --resume)       RESUME=true; shift ;;
    --from)         FORCE_PHASE="$2"; shift 2 ;;
    --no-pr)        DO_PR=false; shift ;;
    --no-tracker)   NO_TRACKER=true; shift ;;
    --plan-max)     PLAN_MAX="$2"; shift 2 ;;
    --build-max)    BUILD_MAX="$2"; shift 2 ;;
    --review-max)   REVIEW_MAX="$2"; shift 2 ;;
    --review-every) REVIEW_EVERY="$2"; shift 2 ;;
    --time-budget)  TIME_BUDGET="$2"; shift 2 ;;
    --branch-cap)   BRANCH_CAP="$2"; shift 2 ;;
    -*)             echo "Unknown option: $1" >&2; exit 1 ;;
    *)              DESIGN_DOC="$1"; shift ;;
  esac
done

[ "${RALPH_NO_TRACKER:-}" = "1" ] && NO_TRACKER=true

mkdir -p "$RALPH_DIR"
START_TS=$(date +%s)
LOG="$RALPH_DIR/ralph-$(date +%Y%m%d-%H%M%S).log"
SESSION_NAME=$(tmux display-message -p '#S' 2>/dev/null || echo "")
# Only tear down the tmux session if the loop was launched into one of its own.
# SKILL.md's launch command sets RALPH_OWNS_TMUX=1. Without this guard, a
# foreground run kills whatever session the operator happens to be sitting in.
OWNS_TMUX="${RALPH_OWNS_TMUX:-0}"

log()  { echo "[$(date '+%H:%M:%S')] $*" | tee -a "$LOG"; }
banner() { log ""; log "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"; log "$*"; log "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"; }
die()  { log "ERROR: $*"; exit 1; }

time_spent() { echo $(($(date +%s) - START_TS)); }
time_exhausted() { [ "$(time_spent)" -ge "$TIME_BUDGET" ]; }

set_phase() { echo "$1" > "$STATE_FILE"; }
get_phase() { cat "$STATE_FILE" 2>/dev/null || echo ""; }

# ── Preflight ────────────────────────────────────────────────────────────────

git rev-parse --git-dir >/dev/null 2>&1 || die "not a git repository. Run this from inside your project."

BASE_BRANCH=""
if [ -f "$STACK_FILE" ]; then
  BASE_BRANCH=$(jq -r '.base_branch // empty' "$STACK_FILE" 2>/dev/null)
fi
if [ -z "$BASE_BRANCH" ]; then
  BASE_BRANCH=$(git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's|^origin/||')
  [ -z "$BASE_BRANCH" ] && BASE_BRANCH=$(git rev-parse --verify --quiet main >/dev/null 2>&1 && echo main || echo master)
fi

if [ "$DO_PR" = true ]; then
  git remote get-url origin >/dev/null 2>&1 || die "no 'origin' remote. Add one, or re-run with --no-pr."
  command -v gh >/dev/null 2>&1 || die "'gh' CLI not found but --no-pr was not passed. Install gh, or re-run with --no-pr."
fi

command -v jq >/dev/null 2>&1 || die "'jq' is required."
command -v claude >/dev/null 2>&1 || die "'claude' CLI not found."

if [ "$RESUME" = false ] && [ -z "$FORCE_PHASE" ]; then
  [ -n "$DESIGN_DOC" ] || die "no design doc given. Usage: .ralph/loop.sh <design-doc>"
  [ -f "$DESIGN_DOC" ] || die "design doc not found: $DESIGN_DOC"
fi

# Recover the design doc on resume
if [ -z "$DESIGN_DOC" ] && [ -f tasks.json ]; then
  DESIGN_DOC=$(jq -r '.spec_file // empty' tasks.json 2>/dev/null)
fi

# Never let the loop commit straight to the base branch
BRANCH=$(git branch --show-current 2>/dev/null || echo "")
if [ -z "$BRANCH" ]; then
  die "detached HEAD. Check out a branch first."
fi
if [ "$BRANCH" = "$BASE_BRANCH" ]; then
  slug=$(basename "${DESIGN_DOC:-feature}" | sed 's/\.[^.]*$//' | tr '[:upper:] ' '[:lower:]-' | sed 's/[^a-z0-9-]//g' | cut -c1-40)
  BRANCH="ralph/${slug:-feature}"
  log "On base branch '$BASE_BRANCH' — creating feature branch '$BRANCH'"
  git checkout -b "$BRANCH" 2>&1 | tee -a "$LOG" || die "could not create branch $BRANCH"
fi

LOOP_TRACKER_URL="${LOOP_TRACKER_URL:-http://localhost:3000}"
if [ "$NO_TRACKER" = false ]; then
  curl -s -o /dev/null --max-time 3 "$LOOP_TRACKER_URL" 2>/dev/null \
    || die "Loop Tracker unreachable at $LOOP_TRACKER_URL. Start it, or re-run with --no-tracker."
  log "Loop Tracker: reachable"
fi

banner "RALPH IMPLEMENT"
log "Design doc:  ${DESIGN_DOC:-<resumed>}"
log "Branch:      $BRANCH  (base: $BASE_BRANCH)"
log "Caps:        plan=$PLAN_MAX review=$REVIEW_MAX review_every=$REVIEW_EVERY time=${TIME_BUDGET}s"
log "PR:          $DO_PR"
log "Log:         $LOG"

# ── Claude runner ────────────────────────────────────────────────────────────

run_claude() {
  local prompt_file="$1"
  local iteration_type="${2:-build}"
  local attempt=1
  local delay=$RETRY_DELAY

  [ -f "$prompt_file" ] || { log "ERROR: prompt not found: $prompt_file"; return 1; }

  while [ $attempt -le $RETRY_MAX ]; do
    log "claude ← $(basename "$prompt_file") (attempt $attempt/$RETRY_MAX)"
    export RALPH_ITERATION_TYPE="$iteration_type"
    export RALPH_DESIGN_DOC="${DESIGN_DOC:-}"

    if claude -p --dangerously-skip-permissions --verbose --output-format=stream-json \
         < "$prompt_file" 2>&1 | stdbuf -oL jq -r '.' | stdbuf -oL tee -a "$LOG"; then
      return 0
    fi

    if tail -n 200 "$LOG" | grep -q "529\|overloaded\|500\|502\|503\|504\|timeout\|ECONNRESET"; then
      log "Retryable error. Waiting ${delay}s..."
      sleep "$delay"
      delay=$((delay * 2))
      attempt=$((attempt + 1))
    else
      log "Non-retryable error from claude."
      return 1
    fi
  done

  log "Max retries exceeded."
  return 1
}

head_sha()    { git rev-parse HEAD 2>/dev/null || echo none; }

push_changes() {
  git remote get-url origin >/dev/null 2>&1 || return 0
  local unpushed
  if git rev-parse --verify -q "origin/$BRANCH" >/dev/null 2>&1; then
    unpushed=$(git log "origin/$BRANCH..HEAD" --oneline 2>/dev/null | wc -l | tr -d ' ')
  else
    # Branch not on the remote yet — everything we have is unpushed.
    unpushed=$(git log --oneline 2>/dev/null | wc -l | tr -d ' ')
  fi
  [ "${unpushed:-0}" -eq 0 ] && return 0
  log "Pushing $unpushed commit(s)..."
  git push origin "$BRANCH" 2>&1 | tee -a "$LOG" \
    || git push -u origin "$BRANCH" 2>&1 | tee -a "$LOG" || true
}

all_tasks_done() {
  [ -f tasks.json ] || return 1
  local total incomplete
  total=$(jq '[.tasks[]] | length' tasks.json 2>/dev/null || echo 0)
  [ "$total" -eq 0 ] && return 1
  incomplete=$(jq '[.tasks[] | select(.done != true)] | length' tasks.json 2>/dev/null || echo 999)
  [ "$incomplete" -eq 0 ]
}

# True when tasks.json holds at least one task still to do. Distinguishes "the planner
# wrote work" from "the planner deliberately wrote none".
has_open_tasks() {
  [ -f tasks.json ] || return 1
  local incomplete
  incomplete=$(jq '[.tasks[] | select(.done != true)] | length' tasks.json 2>/dev/null || echo 0)
  [ "${incomplete:-0}" -gt 0 ]
}

# Tasks a review cycle appended, tagged `"kind": "fix"` by PROMPT_review_plan.md.
# The interim fix round is bounded to these. Its predecessor used all_tasks_done, so the
# first checkpoint review that found a CRIT built every remaining feature task inside the
# review phase — and no later checkpoint ever fired, which defeats --review-every entirely.
count_open_fix_tasks() {
  [ -f tasks.json ] || { echo 0; return; }
  local c
  c=$(jq '[.tasks[] | select(.done != true and .kind == "fix")] | length' tasks.json 2>/dev/null || echo 0)
  echo "${c:-0}"
}

# Done-condition for the fix round: every fix task closed. Feature tasks still open are
# not this round's business — the checkpointed build loop picks them back up.
fix_tasks_done() {
  [ "$(count_open_fix_tasks)" -eq 0 ]
}

tracker_phase() {
  [ "$NO_TRACKER" = true ] && return 0
  local phase="$1"
  local sid="phase-${phase}-$(date +%s)"
  local api="$LOOP_TRACKER_URL/api/events"
  curl -s -X POST "$api" -H "Content-Type: application/json" \
    -d "$(jq -n --arg sid "$sid" --arg pp "$(pwd)" --arg br "$BRANCH" --arg ts "$SESSION_NAME" \
      '{event_type:"session_start", session_id:$sid, project_path:$pp, branch:$br, tmux_session:$ts}')" >/dev/null || true
  curl -s -X POST "$api" -H "Content-Type: application/json" \
    -d "$(jq -n --arg sid "$sid" --arg ph "$phase" \
      '{event_type:"session_end", session_id:$sid, phase:$ph}')" >/dev/null || true
}

# ── Iteration driver ─────────────────────────────────────────────────────────
# run_iterations <max> <prompt> <type> <no_change_limit> <done_check_fn|->
# Exit: 0 done-condition met | 2 converged | 3 max reached | 4 out of time

# ── The mechanical gate ──────────────────────────────────────────────────────
# Until 2026-08-04 this loop read build_command, lint_command and test_command
# out of stack.json and only ever *logged* them. The single signal it used to
# judge an iteration was whether HEAD moved, so "the suite is green" was always
# the build agent's own prose about itself, and ten hours of work produced 87
# review findings with nothing able to contradict it. The agent asserts; the
# loop verifies.
verify_head() {
  local cmd
  for key in build_command lint_command test_command; do
    cmd=$(jq -r ".$key // empty" "$STACK_FILE")
    [ -z "$cmd" ] && continue
    log "verify: $cmd"
    if ! bash -c "$cmd" >>"$LOG" 2>&1; then
      log "VERIFY FAILED: $cmd"
      {
        echo "# The last commit did not pass the stack commands"
        echo
        echo "Failed: \`$cmd\`"
        echo
        echo "This ran after an iteration committed, and the commit was rolled back."
        echo "**Read this before taking a task.** Whatever you were doing last iteration"
        echo "left the tree failing this command; the work is gone but the mistake is not."
        echo "Fix the cause, do not re-attempt the same approach blind, and delete this file"
        echo "in the commit that fixes it."
        echo
        echo '```'
        tail -n 60 "$LOG"
        echo '```'
      } > "$RALPH_DIR/VERIFY_FAILED.md"
      return 1
    fi
  done
  rm -f "$RALPH_DIR/VERIFY_FAILED.md"
  return 0
}

# A branch nobody can review is a branch nobody does review. The phase-2a
# post-mortem: +20,669 insertions over 104 files, re-read in full five times,
# and the reviewer's denominator never shrank. SmartBear's Cisco study puts the
# effective ceiling at 200-400 LOC per review; this is a loose multiple of that,
# meant to force a merge rather than to be precise.
branch_too_large() {
  # 0 disables the cap. That is not a convenience: the phase-2a A/B needs one arm
  # run under attempt 1's conditions, and a run that stops early is not comparable
  # to one that never stopped. Outside an experiment, leave it on.
  [ "$BRANCH_CAP" -eq 0 ] && return 1
  local ins
  ins=$(git diff --numstat "$BASE_BRANCH"...HEAD 2>/dev/null | awk '{s+=$1} END {print s+0}')
  [ "${ins:-0}" -gt "$BRANCH_CAP" ] && { echo "$ins"; return 0; }
  return 1
}

run_iterations() {
  local max="$1" prompt="$2" itype="$3" nochange_limit="$4" done_fn="$5"
  local iter=0 nochange=0

  while true; do
    if time_exhausted; then
      log "Wall-clock budget exhausted after $(time_spent)s"
      return 4
    fi
    if [ "$max" -gt 0 ] && [ "$iter" -ge "$max" ]; then
      log "Reached max iterations ($max) for phase '$itype'"
      return 3
    fi

    iter=$((iter + 1))
    log ""
    log "═══════ $itype — iteration $iter ═══════"

    local before after
    before=$(head_sha)
    run_claude "$prompt" "$itype" || log "Iteration had errors (continuing)"
    after=$(head_sha)

    if [ "$before" = "$after" ]; then
      nochange=$((nochange + 1))
      log "No commit this iteration ($nochange/$nochange_limit)"
      if [ "$nochange" -ge "$nochange_limit" ]; then
        log "Converged: no changes for $nochange_limit consecutive iterations"
        return 2
      fi
    else
      nochange=0
      log "New commit: $after"

      # Verified before it is pushed, so a rollback is a local reset rather than
      # a force-push, and a red commit never reaches the branch the reviewer reads.
      if ! verify_head; then
        log "Rolling back $after — it does not pass the stack commands."
        git reset --hard "$before" >>"$LOG" 2>&1 \
          || git revert --no-edit "$after" >>"$LOG" 2>&1 \
          || log "ERROR: could not roll back $after — the tree needs a human."
        nochange=$((nochange + 1))
        log "Rolled back ($nochange/$nochange_limit consecutive iterations without a kept commit)"
        if [ "$nochange" -ge "$nochange_limit" ]; then
          log "Converged: $nochange_limit iterations produced nothing that verifies"
          return 2
        fi
        sleep 5
        continue
      fi

      local ins
      if ins=$(branch_too_large); then
        log "Branch is +$ins lines against $BASE_BRANCH (cap $BRANCH_CAP)."
        log "Stopping so this can be reviewed and merged before it grows further."
        push_changes
        write_escalation "branch exceeded the review budget (+$ins lines against $BASE_BRANCH)" "0"
        return 5
      fi
    fi

    if [ "$done_fn" != "-" ] && "$done_fn"; then
      log "Done-condition met for phase '$itype'"
      push_changes
      return 0
    fi

    push_changes
    sleep 5
  done
}

# ── Findings counters ────────────────────────────────────────────────────────

# -a on every read below is load-bearing, not defensive. REVIEW_FINDINGS.md
# quotes hostile input verbatim — a spec whose securitySchemes name carries
# ESC[2K, a stored body of C0 bytes — so the file contains control characters as
# a matter of course, and this box ships ugrep as `grep`, which reports no match
# and exit 1 on a file it decides is binary. Measured on the 2026-08-04 cycle-1
# findings: every counter returned empty without -a and the correct value with
# it. A guardrail that reads zero because it could not read at all is the exact
# failure this harness was rebuilt around this morning.
count_findings() {
  [ -f REVIEW_FINDINGS.md ] || { echo 0; return; }
  local c; c=$(grep -ac '^## Finding' REVIEW_FINDINGS.md 2>/dev/null || true); echo "${c:-0}"
}
count_crits() {
  [ -f REVIEW_FINDINGS.md ] || { echo 0; return; }
  local c; c=$(grep -ac '^\- \*\*Severity:\*\* CRIT' REVIEW_FINDINGS.md 2>/dev/null || true); echo "${c:-0}"
}
# Findings the fix loop must not attempt: they need a human decision, or a previous
# fix for the same defect already failed. Counted so the loop can stop instead of spin.
count_design_blocked() {
  [ -f REVIEW_FINDINGS.md ] || { echo 0; return; }
  local c; c=$(grep -ac '^\- \*\*Blocked-by:\*\* design' REVIEW_FINDINGS.md 2>/dev/null || true); echo "${c:-0}"
}
count_repeats() {
  [ -f REVIEW_FINDINGS.md ] || { echo 0; return; }
  local c; c=$(grep -ac '^\- \*\*Repeat-of:\*\* cycle' REVIEW_FINDINGS.md 2>/dev/null || true); echo "${c:-0}"
}

# The guardrails below are all about CRITs — their messages say so. Counting a
# property across findings of *every* severity and comparing that total to the
# CRIT count is an accident waiting for equal numbers, and it happened twice:
# 2026-08-03 (2 design-blocked WARNs, 2 unrelated CRITs) and 2026-08-04 (1
# design-blocked WARN, 1 unrelated CRIT). Both times the loop stopped with
# "every CRIT is blocked on a design decision" over CRITs that were marked
# `Blocked-by: none` and were fixable. These read the two fields per finding.
count_crits_with() {
  [ -f REVIEW_FINDINGS.md ] || { echo 0; return; }
  awk -v field="$1" -v want="$2" '
    # $3 is the value, not $NF: "Repeat-of" reads "cycle 1 findings 3 and 4
    # (partial fix)", so the last field is a word from the prose.
    function flush() { if (sev == "CRIT" && val == want) n++; sev = ""; val = "" }
    /^## Finding/                 { flush() }
    /^\- \*\*Severity:\*\*/       { sev = $3 }
    $0 ~ "^\\- \\*\\*" field ":"  { val = $3 }
    END                           { flush(); print n+0 }
  ' REVIEW_FINDINGS.md
}
count_design_blocked_crits() { count_crits_with "Blocked-by" "design"; }
count_repeat_crits()         { count_crits_with "Repeat-of"  "cycle"; }

# CRITs this branch's own work created, as opposed to ones it has not reached yet.
#
# Repeat-of cannot answer that question. It reads "cycle 2 finding 1" for a
# finding carried from the *input* review as well as for one that survived a fix
# in this run, and during an unfinished phase the first is the normal case: at
# arm B's first checkpoint, four of five CRITs were original findings whose tasks
# were still queued, and the repeat gate escalated on them as "a fix failed".
# That was a counter reading a field that does not mean what the gate assumed —
# the same defect as the severity miscount it replaced.
#
# Introduced-by is the field that does answer it, and it is the one that matters:
# a fix creating a critical is what actually broke arm A, at one to two per cycle.
# Introduced unless the value begins "none". Reviewers write it as a sha and a
# task — "`d1548bf` (task 2)" — or as "none — reproduces on `main`", with the
# reasoning inline. An exact-match on the word "task" read 0 against 18 findings
# that carried a sha, which is why this matches the negative instead: the field
# is prose with a fixed first token, so the first token is the only safe thing to
# key on.
count_introduced_crits() {
  [ -f REVIEW_FINDINGS.md ] || { echo 0; return; }
  awk '
    function flush() { if (sev == "CRIT" && val != "" && val != "none") n++; sev = ""; val = "" }
    /^## Finding/                    { flush() }
    /^\- \*\*Severity:\*\*/        { sev = $3 }
    /^\- \*\*Introduced-by:\*\*/   { val = $3; sub(/^none.*/, "none", val) }
    END                              { flush(); print n+0 }
  ' REVIEW_FINDINGS.md
}

# Whether the review recorded the field at all. A counter that cannot see its
# input must not answer "clear" — that is the defect this whole harness was
# rebuilt around today, and it recurred within hours: Introduced-by was added to
# the finding template, four reviewer subagents composed their own prompts
# without it, and count_introduced_crits read 0 across 8 CRITs and let the run
# continue. Absent is not zero.
findings_record_field() {
  [ -f REVIEW_FINDINGS.md ] || return 1
  grep -aq "^\- \*\*$1:\*\*" REVIEW_FINDINGS.md
}

# Everything the loop could not resolve on its own, in one file for the human.
write_escalation() {
  local reason="$1" cycle="$2"
  {
    echo "# Review escalation"
    echo
    echo "The review loop stopped in cycle $cycle and is handing back to a human."
    echo
    echo "**Reason:** $reason"
    echo
    echo "## CRIT history"
    echo
    echo "| Cycle | CRIT |"
    echo "|---|---|"
    local i=1
    while read -r n; do echo "| $i | $n |"; i=$((i + 1)); done < "$RALPH_DIR/review_history" 2>/dev/null
    echo
    echo "## What needs a decision"
    echo
    if [ "$(count_design_blocked)" -gt 0 ]; then
      echo "These findings are marked \`Blocked-by: design\` — fixing them means choosing a"
      echo "policy the design doc does not state. Decide the policy, amend the design doc,"
      echo "then re-run with \`--from review\`."
      echo
      awk '/^## Finding/{t=$0} /^\- \*\*Blocked-by:\*\* design/{print "- " substr(t,4)}' REVIEW_FINDINGS.md
      echo
    fi
    if [ "$(count_repeats)" -gt 0 ]; then
      echo "These findings are repeats — a previous cycle's fix did not hold. Re-applying the"
      echo "same approach will fail again; they need a different one."
      echo
      awk '/^## Finding/{t=$0} /^\- \*\*Repeat-of:\*\* cycle/{print "- " substr(t,4)}' REVIEW_FINDINGS.md
      echo
    fi
    echo "Full detail in \`REVIEW_FINDINGS.md\`."
  } > REVIEW_ESCALATION.md
  git add REVIEW_ESCALATION.md 2>/dev/null || true
  git commit -q -m "review: escalate to human ($reason)" 2>/dev/null || true
}

# ── Phases ───────────────────────────────────────────────────────────────────

phase_stack() {
  banner "PHASE 1/6 — STACK DETECTION"
  set_phase stack
  tracker_phase stack
  run_claude "$RALPH_DIR/PROMPT_stack.md" "stack" || true
  push_changes

  [ -f "$STACK_FILE" ] || { log "ERROR: $STACK_FILE was not produced"; return 1; }
  jq -e '.test_command' "$STACK_FILE" >/dev/null 2>&1 || { log "ERROR: $STACK_FILE has no test_command"; return 1; }

  local detected_base
  detected_base=$(jq -r '.base_branch // empty' "$STACK_FILE")
  [ -n "$detected_base" ] && BASE_BRANCH="$detected_base"
  log "Stack: $(jq -r '.language + " / " + (.framework // "none")' "$STACK_FILE")"
  log "Tests: $(jq -r '.test_command' "$STACK_FILE")"
  log "Base branch: $BASE_BRANCH"
  return 0
}

phase_plan() {
  banner "PHASE 2/5 — PLAN"
  set_phase plan
  tracker_phase plan

  run_iterations "$PLAN_MAX" "$RALPH_DIR/PROMPT_plan.md" "plan" 2 "-"
  local rc=$?
  # Converged (2) and max-reached (3) are both acceptable outcomes for planning.
  # Only a time-out is fatal here.
  [ "$rc" -eq 4 ] && return 4

  [ -f tasks.json ] || { log "ERROR: plan phase produced no tasks.json"; return 1; }
  local n
  n=$(jq '[.tasks[]] | length' tasks.json 2>/dev/null || echo 0)
  [ "$n" -gt 0 ] || { log "ERROR: tasks.json contains no tasks"; return 1; }
  log "Plan ready: $n task(s)"
  return 0
}

phase_build() {
  banner "PHASE 3/6 — BUILD"
  set_phase build
  tracker_phase build

  local max="$BUILD_MAX"
  if [ "$max" -eq 0 ]; then
    local n; n=$(jq '[.tasks[] | select(.done != true)] | length' tasks.json 2>/dev/null || echo 5)
    max=$((n * 2))
    [ "$max" -lt 10 ] && max=10
    log "Build cap derived from $n incomplete task(s): $max iterations"
  fi

  # REVIEW_EVERY=0 keeps the original behaviour: build every task, then review once.
  if [ "$REVIEW_EVERY" -le 0 ]; then
    run_iterations "$max" "$RALPH_DIR/PROMPT_build.md" "build" 3 all_tasks_done
    local rc=$?
    case $rc in
      0) log "All tasks complete."; return 0 ;;
      4) return 4 ;;
      *) log "Build phase ended without completing all tasks (rc=$rc)"; return 1 ;;
    esac
  fi

  # Checkpointed build: review every REVIEW_EVERY iterations instead of hoarding
  # findings until the end. A review that runs once after N tasks reports N tasks'
  # worth of drift at once, which is how a fix pass turns into an escalation.
  local rounds=$(( (max + REVIEW_EVERY - 1) / REVIEW_EVERY ))
  local round=0
  while [ "$round" -lt "$rounds" ]; do
    round=$((round + 1))
    log "═══════ build checkpoint $round/$rounds (review every $REVIEW_EVERY) ═══════"

    run_iterations "$REVIEW_EVERY" "$RALPH_DIR/PROMPT_build.md" "build" 3 all_tasks_done
    local rc=$?
    case $rc in
      0) log "All tasks complete."; return 0 ;;
      3) : ;;   # hit the checkpoint with tasks outstanding — review, then continue
      4) return 4 ;;
      *) log "Build phase ended without completing all tasks (rc=$rc)"; return 1 ;;
    esac

    log "Checkpoint reached with tasks outstanding — running an interim review"
    phase_review
    local review_rc=$?
    # Restore the phase marker so --resume comes back to build, not review.
    set_phase build
    tracker_phase build
    [ "$review_rc" -ne 0 ] && return "$review_rc"
  done

  log "Build phase exhausted its iteration cap ($max) with tasks outstanding"
  return 1
}

# Findings are the most valuable thing a run produces and the working files at the repo
# root are scratch: the next cycle deletes REVIEW_FINDINGS.md before the reviewer writes a
# new one. Archive every cycle under a timestamp so nothing is ever overwritten, and later
# work can cite a stable path instead of a file that changes under it.
REVIEW_ARCHIVE_DIR="docs/review"

archive_review() {
  local cycle="$1" stamp
  stamp=$(date -u +%Y%m%d-%H%M%S)
  mkdir -p "$REVIEW_ARCHIVE_DIR"
  [ -f REVIEW_FINDINGS.md ] \
    && cp REVIEW_FINDINGS.md "$REVIEW_ARCHIVE_DIR/${stamp}-cycle${cycle}-findings.md"
  [ -f REVIEW_ESCALATION.md ] \
    && cp REVIEW_ESCALATION.md "$REVIEW_ARCHIVE_DIR/${stamp}-cycle${cycle}-escalation.md"
  # Commit the archive by path so it survives even if the run dies here, and so it cannot
  # be swept into an unrelated commit by whatever the next iteration stages.
  if [ -n "$(git status --porcelain "$REVIEW_ARCHIVE_DIR" 2>/dev/null)" ]; then
    git add "$REVIEW_ARCHIVE_DIR" 2>/dev/null || true
    git commit -q -m "docs: archive review cycle $cycle findings" -- "$REVIEW_ARCHIVE_DIR" \
      2>/dev/null || true
  fi
  log "Archived cycle $cycle review to $REVIEW_ARCHIVE_DIR/${stamp}-cycle${cycle}-*.md"
}

phase_review() {
  banner "PHASE 4/6 — REVIEW"
  set_phase review
  tracker_phase review

  # Per-run CRIT tally. Must start empty: the no-progress check indexes it by cycle
  # number, so counts left over from an earlier run would compare against the wrong cycle.
  : > "$RALPH_DIR/review_history"
  rm -f REVIEW_FINDINGS_PREV.md

  local cycle=0
  while true; do
    if time_exhausted; then log "Wall-clock budget exhausted"; return 4; fi
    if [ "$cycle" -ge "$REVIEW_MAX" ]; then
      log "Reached max review cycles ($REVIEW_MAX) with CRIT findings outstanding"
      return 1
    fi

    cycle=$((cycle + 1))
    export RALPH_REVIEW_CYCLE=$cycle
    log ""
    log "═══════ review cycle $cycle/$REVIEW_MAX ═══════"

    # Hand the previous cycle's findings to the reviewer so it can flag repeats — a defect
    # that survived a fix is the strongest signal the approach is wrong.
    # Only from cycle 2 on: a REVIEW_FINDINGS.md left over from an earlier *run* was never
    # fixed by this run, so treating it as "previously fixed" would mark unfixed findings as
    # repeats and escalate on the first cycle.
    if [ "$cycle" -gt 1 ] && [ -f REVIEW_FINDINGS.md ]; then
      cp REVIEW_FINDINGS.md REVIEW_FINDINGS_PREV.md
    fi
    rm -f REVIEW_FINDINGS.md
    local review_ok=true
    run_claude "$RALPH_DIR/PROMPT_review.md" "review" || { review_ok=false; log "Review iteration had errors"; }
    push_changes

    # A failed review that left no findings file is NOT a clean pass. The file is deleted
    # above, so "reviewed and found nothing" and "review never ran" are otherwise identical
    # — and the second one would sail through to PR claiming zero CRIT findings.
    if [ "$review_ok" = false ] && [ ! -f REVIEW_FINDINGS.md ]; then
      log "Review failed and wrote no findings file — this cycle certifies nothing."
      if [ "$cycle" -lt "$REVIEW_MAX" ]; then
        log "Retrying the review cycle."
        sleep 5
        continue
      fi
      log "Reached max review cycles ($REVIEW_MAX) without a completed review."
      unset RALPH_REVIEW_CYCLE
      return 1
    fi

    local findings crits blocked repeats blocked_crits repeat_crits introduced_crits
    findings=$(count_findings)
    crits=$(count_crits)
    blocked=$(count_design_blocked)
    repeats=$(count_repeats)
    blocked_crits=$(count_design_blocked_crits)
    repeat_crits=$(count_repeat_crits)
    introduced_crits=$(count_introduced_crits)
    echo "$crits" >> "$RALPH_DIR/review_history"
    log "Findings: $findings total, $crits CRIT ($blocked design-blocked, $repeats repeat)"
    log "Of the CRITs: $blocked_crits design-blocked, $repeat_crits repeat, $introduced_crits self-inflicted"
    archive_review "$cycle"

    # ── Guardrails: stop rather than spin ────────────────────────────────────
    # Each of these means another cycle cannot help. Escalating beats burning the cap.
    #
    # All of them are scoped to CRITs. A design-blocked or repeating WARN is real
    # information for the human, but it is not a reason to stop with a fixable CRIT
    # on the floor — which is what the whole-file counts did, twice.

    if [ "$blocked_crits" -gt 0 ] && [ "$blocked_crits" -eq "$crits" ]; then
      log "All $crits CRIT finding(s) need a design decision this loop cannot make."
      write_escalation "every CRIT is blocked on a design decision" "$cycle"
      push_changes; unset RALPH_REVIEW_CYCLE; return 5
    fi

    if [ "$crits" -gt 0 ] && ! findings_record_field "Introduced-by"; then
      log "The review recorded no Introduced-by field, so the self-inflicted check could not run."
      log "Not treating that as zero: $crits CRIT finding(s) are unclassified."
      write_escalation "the review omitted Introduced-by; whether the fixes created these $crits CRIT(s) is unknown" "$cycle"
      push_changes; unset RALPH_REVIEW_CYCLE; return 5
    fi

    if [ "$introduced_crits" -gt 0 ]; then
      log "$introduced_crits CRIT finding(s) were introduced by this branch's own fixes."
      write_escalation "$introduced_crits CRIT finding(s) created by the fixes themselves" "$cycle"
      push_changes; unset RALPH_REVIEW_CYCLE; return 5
    fi

    # A repeat is no longer grounds to stop. Mid-phase it usually means "this
    # finding's task has not run yet", which is the plan working, not failing.
    if [ "$repeat_crits" -gt 0 ]; then
      log "Note: $repeat_crits CRIT finding(s) carry Repeat-of — check they are queued, not stalled."
    fi

    # Non-CRIT versions of the two conditions above: worth saying out loud every
    # cycle, not worth stopping for. They reach the human in REVIEW_FINDINGS.md,
    # which is archived under docs/review/ whether or not an escalation is written.
    if [ "$((blocked - blocked_crits))" -gt 0 ]; then
      log "Note: $((blocked - blocked_crits)) non-CRIT finding(s) need a design decision — see REVIEW_FINDINGS.md."
    fi
    if [ "$((repeats - repeat_crits))" -gt 0 ]; then
      log "Note: $((repeats - repeat_crits)) non-CRIT finding(s) repeat after a previous fix."
    fi

    # No progress: this cycle found at least as many CRITs as the last one. Fixing is
    # keeping pace with discovery at best, and the cap will not change that.
    local prev
    prev=$(sed -n "$((cycle - 1))p" "$RALPH_DIR/review_history" 2>/dev/null)
    if [ "$cycle" -gt 1 ] && [ -n "$prev" ] && [ "$crits" -ge "$prev" ] && [ "$crits" -gt 0 ]; then
      log "No progress: cycle $cycle found $crits CRIT vs $prev in cycle $((cycle - 1))."
      write_escalation "CRIT count did not fall between cycles ($prev → $crits)" "$cycle"
      push_changes; unset RALPH_REVIEW_CYCLE; return 5
    fi

    if [ "$crits" -eq 0 ]; then
      if [ "$findings" -eq 0 ]; then
        log "Clean pass — no findings."
      else
        log "No CRIT findings. $findings WARN/INFO left in REVIEW_FINDINGS.md for human review."
      fi
      unset RALPH_REVIEW_CYCLE
      return 0
    fi

    local fixable=$((crits - blocked_crits))
    log "Planning fixes for $fixable of $crits CRIT finding(s) ($blocked_crits need a design decision)..."
    run_claude "$RALPH_DIR/PROMPT_review_plan.md" "review_plan" || log "Review-plan had errors (continuing)"
    push_changes

    # The planner writes no tasks when every CRIT was skipped as design-blocked or a repeat.
    # Without this check the fix phase would iterate against an already-satisfied done
    # condition and the cycle would repeat identically until the cap.
    if ! has_open_tasks; then
      log "Review-plan produced no actionable tasks — nothing here is fixable without a human."
      write_escalation "no CRIT finding was actionable without a design decision" "$cycle"
      push_changes; unset RALPH_REVIEW_CYCLE; return 5
    fi

    # Bound the fix round to the tasks this cycle created. A fix task may need a second
    # iteration, so allow two apiece rather than exactly one.
    local open_fixes rc
    open_fixes=$(count_open_fix_tasks)
    if [ "$open_fixes" -gt 0 ]; then
      log "Fixing $open_fixes task(s) from cycle $cycle."
      run_iterations "$((open_fixes * 2))" "$RALPH_DIR/PROMPT_build.md" "review_fix" 3 fix_tasks_done
      rc=$?
    else
      # No task carries kind:"fix" — a tasks.json written before this field existed, or a
      # planner that dropped it. Falling back to the whole backlog keeps the fixes from
      # being skipped; it costs the checkpoint rhythm, which is the old behaviour anyway.
      log "No kind:\"fix\" tasks present — falling back to the full backlog for this round."
      run_iterations 0 "$RALPH_DIR/PROMPT_build.md" "review_fix" 3 all_tasks_done
      rc=$?
    fi
    [ "$rc" -eq 4 ] && return 4
    push_changes
    sleep 5
  done
}

# Between the last fix and the pull request: the branch is made true. Tests earn
# their place, the shipped documents describe the binary that exists, and
# CLAUDE.md describes this code rather than code that was deleted. Every later
# iteration starts by reading those files, so a stale line in one is a wrong
# instruction delivered with authority to everyone who comes after — CLAUDE.md
# went 93 to 663 lines across one phase because every iteration added and none
# removed. Behaviour changes are out of scope here and get reported, not made.
phase_consolidate() {
  banner "PHASE 5/6 — CONSOLIDATE"
  set_phase consolidate
  tracker_phase consolidate

  local before after
  before=$(head_sha)
  run_claude "$RALPH_DIR/PROMPT_consolidate.md" "consolidate" || log "Consolidate had errors (continuing)"
  after=$(head_sha)

  if [ "$before" != "$after" ] && ! verify_head; then
    log "Rolling back $after — the consolidation commit does not pass the stack commands."
    git reset --hard "$before" >>"$LOG" 2>&1 || true
    return 0   # a failed tidy-up must not block the PR the built work earned
  fi

  push_changes
  return 0
}

phase_pr() {
  banner "PHASE 6/6 — PULL REQUEST"
  set_phase pr
  tracker_phase pr

  push_changes
  git tag -f "ralph-complete/$BRANCH" >/dev/null 2>&1 || true
  git push origin "ralph-complete/$BRANCH" --force >/dev/null 2>&1 || true

  local existing
  existing=$(gh pr view "$BRANCH" --json url -q .url 2>/dev/null || true)
  if [ -n "$existing" ]; then
    log "PR already exists: $existing"
    echo "$existing" > "$RALPH_DIR/pr_url.txt"
    return 0
  fi

  rm -f "$RALPH_DIR/pr_url.txt"
  export RALPH_BASE_BRANCH="$BASE_BRANCH"
  run_claude "$RALPH_DIR/PROMPT_pr.md" "pr" || log "PR iteration had errors"

  if [ -s "$RALPH_DIR/pr_url.txt" ]; then
    log "PR opened: $(cat "$RALPH_DIR/pr_url.txt")"
    return 0
  fi
  log "ERROR: no PR URL recorded in $RALPH_DIR/pr_url.txt"
  return 1
}

# ── Orchestration ────────────────────────────────────────────────────────────

PHASES=(stack plan build review consolidate pr)

start_phase="stack"
if [ -n "$FORCE_PHASE" ]; then
  start_phase="$FORCE_PHASE"
elif [ "$RESUME" = true ]; then
  start_phase=$(get_phase)
  [ -z "$start_phase" ] && start_phase="stack"
  log "Resuming from phase: $start_phase"
fi

started=false
final_rc=0

for phase in "${PHASES[@]}"; do
  [ "$phase" = "$start_phase" ] && started=true
  [ "$started" = false ] && continue
  [ "$phase" = "pr" ] && [ "$DO_PR" = false ] && { log "Skipping PR phase (--no-pr)"; break; }

  "phase_$phase"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    case $rc in
      4) banner "STOPPED — wall-clock budget exhausted in phase '$phase'"
         log "State saved. Resume with: $RALPH_DIR/loop.sh --resume"
         final_rc=2 ;;
      5) banner "ESCALATED — review needs a human decision"
         log "Read REVIEW_ESCALATION.md, then decide."
         log "The loop stopped on purpose: another cycle could not have resolved this."
         log "After amending the design, resume with: $RALPH_DIR/loop.sh --from review"
         final_rc=3 ;;
      *) banner "STOPPED — phase '$phase' did not complete (rc=$rc)"
         log "State saved. Resume with: $RALPH_DIR/loop.sh --resume"
         final_rc=2 ;;
    esac
    break
  fi
done

if [ "$final_rc" -eq 0 ]; then
  set_phase done
  banner "COMPLETE"
  [ -s "$RALPH_DIR/pr_url.txt" ] && log "PR: $(cat "$RALPH_DIR/pr_url.txt")"
  [ -f REVIEW_FINDINGS.md ] && log "Note: non-CRIT findings remain in REVIEW_FINDINGS.md"
fi

log "Elapsed: $(($(time_spent) / 60))m"
log "Log: $LOG"

if [ "$OWNS_TMUX" = "1" ] && [ -n "$SESSION_NAME" ]; then
  sleep 2
  tmux kill-session -t "$SESSION_NAME" 2>/dev/null || true
fi

exit "$final_rc"
