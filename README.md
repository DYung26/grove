# 🌳 Grove

> *A grove is a small group of trees growing closely together, typically without dense underbrush.*

`git worktree` lets you grow more than one tree from the same repo — a branch checked out here, another checked out there, all sharing one root. Grove is what keeps that grove tidy: it makes sure every new tree gets what it needs to grow *fast*, without each one dragging in its own tangled undergrowth of `node_modules`.

## The problem

Worktrees are one of git's best features and one of its most annoying to actually live with. Every new worktree starts bare — no `node_modules`, no `target`, no `.venv` — so before you can do anything you're sitting through a full dependency install, again, for a branch you might use for twenty minutes. Multiply that by however many worktrees you keep around, and the "just check out a quick branch to review this PR" workflow stops being quick.

The obvious fix — copy the dependency dirs over — trades install time for disk space and copy time instead, which barely feels like a fix at all once `node_modules` is a few gigabytes.

## The moat

Grove clones dependency directories into every new worktree using **copy-on-write reflinks** (`FICLONE`) instead of a real copy. The new worktree's `node_modules` looks and behaves like a completely independent directory, but until something inside it actually changes, it shares the same disk blocks as the original. You get a full, isolated dependency tree in the time it takes to clone a file's metadata — not its contents.

That's the part other worktree tooling doesn't do. Most tools stop at `git worktree add` with a nicer interface. Grove treats "does this new worktree actually have working dependencies yet" as the real problem, and solves it three layers deep:

- **Native CoW where the filesystem already supports it** (Btrfs, XFS with reflink, APFS) — no setup, it just works.
- **A self-provisioned Btrfs loopback pool as a fallback** on filesystems that don't support reflink natively (ext4, most of Linux by default). Grove sets this up itself — installs `btrfs-progs`, creates the image, mounts it, wires up `fstab` — so you get CoW behavior even on a filesystem that was never built for it. Reflink can never cross filesystems, though, so `grove pool init` also moves the *current repo's own* dependency dirs into the pool (symlinked back into place) the first time it runs — without that, cloning from an ext4 main repo into a Btrfs-backed worktree could still never actually reflink, pool or no pool.
- **Honest degradation when neither is available** — hardlink or full-copy, chosen by you, never silently.

Everything downstream of that — tracking worktrees, migrating pre-existing ones, catching drift, and reporting the actual space saved — exists to make the CoW cloning actually reliable to live with day to day, not just a neat trick for the happy path.

## Install

```bash
git clone https://github.com/dyung/grove.git
cd grove
go build -o grove .
mv grove /usr/local/bin/   # or anywhere on your $PATH
```

Requires Go 1.22+. On Linux, `grove pool init` will offer to install `btrfs-progs` and `rsync` for you the first time it needs them — no manual setup required.

## Quick start

```bash
cd my-project
grove create feature-x        # new worktree, deps cloned via CoW
grove open feature-x          # drop into a subshell inside it
grove list                    # see every worktree Grove is tracking
grove remove feature-x        # done with it — clean it up
```

## Commands

| Command | What it does |
|---|---|
| `grove create <branch>` | Creates a new worktree for `branch` (making the branch too, if it doesn't exist yet) and clones its dependency dirs in via reflink, falling back gracefully if the destination can't support it. |
| `grove list` | Lists every worktree Grove is currently tracking. |
| `grove open <name-or-branch>` | Drops you into a subshell rooted at the worktree, matched against Grove's tracked name first and falling back to any git branch (works for the main repo too). `--print` prints the bare path instead, for `cd $(grove open <name-or-branch> --print)`. |
| `grove rename <old-name> <new-name>` | Renames a tracked worktree: moves its directory via `git worktree move` and updates the registry to match, so its branch and git state stay correctly linked at the new path. |
| `grove remove <branch>` (alias `rm`) | Removes the worktree and stops tracking it. `--force` removes it even with uncommitted changes. |
| `grove prune` | Removes every tracked worktree whose branch has since been deleted. `--dry-run` reports what would go without touching anything. |
| `grove adopt <path>` | Starts tracking a worktree that already exists but that Grove didn't create — an old manual checkout, or one from before you started using Grove. Registry-only; doesn't touch dependency dirs. |
| `grove repair <name>` (alias `sync-deps`) | Retrofits CoW onto an already-tracked worktree: migrates the worktree's own directory into the pool first if it's still on a plain, non-reflinkable filesystem (the case for anything created before the pool existed, or adopted from a pre-Grove layout), then retrofits its dependency dirs the same way `adopt` leaves them or a fallback previously did. |
| `grove status` (alias `doctor`) | Reports drift: tracked worktrees whose deps aren't fully reflinked (repair candidates), and git worktrees on disk that Grove isn't tracking at all (adopt candidates). Doesn't fix anything itself — just tells you what needs attention. |
| `grove savings` (alias `du`) | Shows how much disk space CoW is actually saving: each tracked worktree's dependency-dir size on disk vs. what it would be without CoW, which clone mode (reflink/hardlink/copy/n/a) it's using, and a total across every worktree. |
| `grove pool init` | Sets up the Btrfs loopback pool used for CoW on filesystems without native reflink support, and moves the current repo's own dependency dirs into it (symlinked back into place) so they become a valid reflink source. Idempotent, safe to re-run. `--size` sets the size in GiB. |
| `grove pool status` | Shows whether the pool is currently mounted. |

Both `grove create` and `grove repair` also take:

- `--from <worktree-or-path>` — clone dependency dirs from a different worktree instead of the main repo. Useful when a sibling worktree already has richer state than the main repo does, or one that was adopted with a plain (non-reflinked) copy of its own. If that worktree isn't already pool-resident, Grove migrates its dependency dirs into the pool first (same as `grove pool init` does for the main repo) so the clone can actually reflink instead of silently falling back to hardlink/copy.
- `grove create` additionally takes `--no-deps` to skip dependency cloning entirely.

## Configuring dependency dirs

Grove already knows the common cases — `node_modules` for npm, `target` for Cargo, `.venv` for Python, nothing extra for pnpm (its `node_modules` is mostly symlinks into a shared store already, so cloning it wins nothing). For anything else, drop a `.grove.json` in the repo root:

```json
{
  "dependency_dirs": ["vendor"],
  "exclude_dependency_dirs": ["target"]
}
```

`dependency_dirs` adds to the built-in set for your project type; `exclude_dependency_dirs` removes entries even if Grove would normally include them. Commit this file — it describes the project, not your local machine.

## Why "Grove"

A `git worktree` is, in git's own words, a tree — a working copy of a repo checked out to some path. A repo that grows several of them isn't a forest; it's smaller and more deliberate than that, all rooted in the same place. That's a grove. And the actual definition Grove is named after is doing double duty on purpose: *"typically without dense underbrush"* is the whole pitch. Every other worktree tool lets the grove grow; this one keeps the underbrush — the duplicated, wasted, redundant copies of your dependencies — from ever taking root.
