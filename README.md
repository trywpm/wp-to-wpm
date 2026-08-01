# wp-to-wpm
 
Continuously mirrors WordPress.org plugins and themes from their canonical
Subversion repositories into the [wpm](https://wpm.so) package registry.
 
Every plugin and theme on wp.org becomes a wpm package. Every tagged release in
its SVN tree becomes a published version of that package. The mirror is
append-only and runs unattended.
 
## Design
 
This pipeline mirrors 50K+ packages and roughly 1M versions with no database, no
servers, and no dedicated state store.
 
**State is git.** The allowlists, the closure lists, and the SVN revision
pointers are files under `state/`, committed back to `main` by whichever
workflow changed them. `git log -- state/plugin_last_rev` is the complete
migration history. Recovery from a bad state file is `git checkout`. Update
commits carry catalog counts in the body, so the diff between two days doubles
as an anomaly report.
 
**Compute is GitHub Actions, serialized by one concurrency group.** All three
workflows declare the same group, so at most one runs at a time. Every reader
and writer of the state files is serialized by the platform, which removes a
class of race conditions without a lock service or leader election.
 
**Scheduling is a Cloudflare Worker, not `on: schedule`.** Actions cron drifts
under load, drops ticks, and fires blind. It cannot check what is already
running before it triggers. The worker holds the cron instead, queries the
Actions API for in-flight runs, and only then dispatches via
`workflow_dispatch`. A single KV key with a 23-hour TTL makes the daily
revalidation idempotent across missed, delayed, or duplicated ticks.
 
The result is an audit trail for every state change, no infrastructure to
operate, and a recovery story that is usually "wait for the next tick".
 
Full architecture and operational documentation lives in [DOCS.md](DOCS.md).
 
## How migration happens
 
Packages are discovered automatically. A scheduled job watches the wp.org SVN
repositories for new revisions and publishes each new tag to wpm as soon as it
lands upstream. There is no manual step for a typical release. If your plugin or
theme is active on wp.org, you do not need to do anything; your next tagged
release will be picked up within minutes.
 
The catalog of eligible packages is refreshed twice a day directly from wp.org,
so newly-published plugins and themes are added automatically.
 
## Requesting a specific package
 
If a package is on wp.org but is not appearing on wpm and you would like it
added, please [open an issue](https://github.com/trywpm/wp-to-wpm/issues/new) with:
 
- The plugin or theme slug (the name as it appears in the wp.org URL).
- A short note on why it should be migrated.
A maintainer will look at it and add it manually. Common reasons a package is
not yet in wpm:
 
- It has not had a tagged release in a long time, so the automatic discovery has
  not seen it.
- It was previously marked closed on wp.org and our cached classification is out
  of date.
- Its slug conflicts with another package and needs human resolution.
## Requesting removal
 
If you are the author of a plugin or theme that has been published to wpm and
you would like it removed, [open an issue](https://github.com/trywpm/wp-to-wpm/issues/new). A short
explanation helps but is not required.
 
## Naming conflicts
 
Some plugin and theme slugs clash with each other on wp.org (the same name is
used for both a plugin and a theme). Those entries are tracked in
`state/conflicts.json` and excluded from automatic migration. If you are the
author of one side of a conflict and want your package published under the
original name, open an issue.
 
## Documentation
 
Full architecture and operational documentation lives in [DOCS.md](DOCS.md).
