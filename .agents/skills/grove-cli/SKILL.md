---
name: grove-cli
description: Reference for the Grove CLI, its worktree and pool commands, flags, state model, and safety boundaries. Use when invoking or interpreting Grove commands.
---

# Grove CLI Reference

## At a glance

- Covers the Grove command-line interface and its observable command behavior.
- Run Grove from the repository whose worktrees you want to manage.
- A new branch starts at the current checkout's HEAD; Grove does not silently choose the default branch.
- The create/repair --from option selects a dependency source, not a Git branch base.
- Grove distinguishes tracked worktrees, Git worktrees it does not track, stale registry entries, and recorded backups.
- Commands that remove worktrees, replace dependency trees, or repair the pool can destroy data; read the safety notes before using them.
- Operational worktree procedures remain in the central skills repository; this skill is the command reference.

## Activation contract

Before acting under this skill, read this entire SKILL.md from the first line to the last. The summary above is orientation only; it does not replace the complete instructions below. If a required command, permission, or environment is unavailable, stop and report the exact blocker rather than guessing.

## Command model

Grove wraps Git worktrees and manages a registry, dependency-directory cloning, shared local-only paths, and an optional Btrfs reflink pool. It does not replace Git: branch history and worktree membership remain Git state; Grove's registry adds its own tracking and environment state.

Run commands from the intended repository. Use the repo root and current branch deliberately.

## Command catalog

| Command | Syntax | Purpose |
|---|---|---|
| Create | grove create <branch> [--name NAME] [--from SOURCE] [--no-deps] | Create a branch worktree. If the branch does not exist, Grove asks before creating it from the current checkout's HEAD. |
| List | grove list | List worktrees in Grove's registry. It is not a complete inventory of every Git worktree. |
| Open | grove open <name-or-branch> [--print] | Open a tracked worktree in a subshell; --print emits its path. |
| Adopt | grove adopt <path> [--name NAME] | Add an existing branch-based Git worktree to Grove's registry. Registry-only; it does not migrate or clone dependencies. |
| Rename | grove rename <old-name> <new-name> | Move a tracked worktree and update its Grove name. It does not rename the Git branch. |
| Repair | grove repair <name> [--from SOURCE] [--force] | Retrofit dependency directories for CoW cloning. May migrate the worktree container and replace dependency trees. Alias: sync-deps. |
| Share | grove share [name] | Link configured local-only paths into one or all tracked worktrees. These are shared symlinks, not independent copies. |
| Clean | grove clean [name] [--force] | Review worktrees and choose one to remove, or remove a named worktree. |
| Remove | grove remove <name> [--force] | Remove a tracked worktree. Alias: rm. Branch deletion is a separate prompt. |
| Prune | grove prune [--dry-run] | Reconcile stale registry/Git metadata when a recorded worktree directory is already absent. Does not delete live directories. |
| Status | grove status | Diagnose drift, unhealthy worktrees, untracked Git worktrees, dangling paths, and backups. Alias: doctor. |
| Savings | grove savings | Report CoW disk savings. Alias: du. |
| Cleanup | grove cleanup [--yes] | Remove recorded .grove-bak backups. Interactive by default. |
| Completion | grove completion <bash|zsh|fish|powershell> | Print a shell completion script. |
| Install completions | grove install-completions [shell] [--shell SHELL] [--force] | Install shell completions; replacing an existing file requires --force. |
| Pool init | grove pool init [--size N] | Create and mount the fallback Btrfs pool when native reflinks are unavailable. |
| Pool status | grove pool status | Inspect pool mount/writability and this repository's orphan candidates. |
| Pool migrate | grove pool migrate | Convert the retired whole-container pool layout to per-worktree subvolumes. |
| Pool resize | grove pool resize <+N|-N|N> | Grow by N GiB, shrink by N GiB, or set total size to N GiB. |
| Pool repair | grove pool repair [--force] | Recover a Btrfs pool forced read-only by metadata corruption. |

## Important option semantics

### Branch base versus dependency source

- grove create creates a missing branch from the HEAD of the worktree where the command is run.
- It does not automatically base the new branch on main, master, develop, or the remote default branch.
- --from SOURCE changes where dependency-directory contents are cloned from. It does not change the Git branch start point.
- --name changes the Grove worktree name/path; it does not change the branch name.
- --no-deps skips dependency-directory cloning entirely.

Before creating a feature worktree, fetch and select the intended base commit using the repository's Git workflow. Verify the branch SHA reported by Grove and then verify HEAD inside the new worktree.

### Dependency and shared-path behavior

- grove repair can migrate a worktree into the pool and re-clone configured dependency directories. The normal path preserves old dependency trees as .grove-bak; --force skips that safety rename and can remove the existing destination.
- grove share links local-only paths from the main repository using symlinks. Changes are visible across every worktree sharing that path.
- Grove's default shared paths include untracked environment files and docs/_local. Check .grove.json before assuming a path is private to one worktree.
- grove adopt only adds registry tracking. It does not make an existing worktree reflinked or repair its dependencies.

## Destructive-operation boundaries

- grove clean and grove remove delete worktree directories. They offer separate local-branch deletion; an unmerged branch requires a separate force-delete confirmation.
- --force on clean/remove allows removal despite uncommitted changes. On remove, it also skips confirmation before deleting a corrupted pool subvolume. Inspect the full worktree state first.
- grove prune is for stale records whose directories are already missing. It is not a general cleanup command.
- grove cleanup removes recorded .grove-bak backups. Review each backup before using --yes.
- grove pool init can install packages, create/mount a sparse Btrfs image, edit /etc/fstab, and migrate dependency directories.
- grove pool resize +N grows the pool and consumes backing-filesystem space. -N shrinks it; a bare N sets total size.
- grove pool repair is a last-resort operation. If remount recovery fails, pool recreation can destroy every pool-resident worktree container and reflinked dependency tree across repositories. Never use --force as a routine repair.

## Inspection and recovery cues

- grove list shows Grove's registry; git worktree list --porcelain shows Git's worktree records. Compare both.
- grove status is diagnostic and does not repair anything.
- grove pool status is repository-scoped; it is not proof that the whole machine has no orphaned subvolumes.
- A .grove-bak, .grove-repair-tmp, dangling .wt symlink, missing pool subvolume, or mismatched registry entry indicates possible partial state. Inspect before retrying a failed mutation.
- Storage protection checks both backing filesystem and pool capacity. Do not raise thresholds simply to bypass a block.
- Grove serializes mutations with a non-blocking storage lock. If a mutation is already running, wait rather than launching another.

For workflow decisions, safe worktree creation, cleanup authorization, and multi-repository pool recovery, use the central grove-worktree-management and grove-cleanup skills. This reference does not authorize destructive operations.

## Diagnostic commands

~~~bash
grove list
grove status
git worktree list --porcelain
git status --short --branch
cat .grove.json
cat .grove/registry.json
grove pool status
grove savings
df -hT ~/.grove ~/.grove/pool-mount
df -ih ~/.grove ~/.grove/pool-mount
~~~
