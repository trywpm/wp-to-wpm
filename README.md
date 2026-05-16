# wp-to-wpm

Continuously mirrors WordPress.org plugins and themes from their canonical Subversion repositories into the [wpm](https://wpm.so) package registry.

Every plugin and theme on wp.org becomes a wpm package. Every tagged release in its SVN tree becomes a published version of that package. The mirror is append-only and runs unattended.

## How migration happens

Packages are discovered automatically. A scheduled job watches the wp.org SVN repositories for new revisions and publishes each new tag to wpm as soon as it lands upstream. There is no manual step for a typical release. If your plugin or theme is active on wp.org, you do not need to do anything; your next tagged release will be picked up within minutes.

The catalog of eligible packages is refreshed twice a day directly from wp.org, so newly-published plugins and themes are added automatically.

## Requesting a specific package

If a package is on wp.org but is not appearing on wpm and you would like it added, please [open an issue](../../issues/new) with:

- The plugin or theme slug (the name as it appears in the wp.org URL).
- A short note on why it should be migrated.

A maintainer will look at it and add it manually. Common reasons a package is not yet in wpm:

- It has not had a tagged release in a long time, so the automatic discovery has not seen it.
- It was previously marked closed on wp.org and our cached classification is out of date.
- Its slug conflicts with another package and needs human resolution.

## Requesting removal

If you are the author of a plugin or theme that has been published to wpm and you would like it removed, [open an issue](../../issues/new). A short explanation helps but is not required.

## Naming conflicts

Some plugin and theme slugs clash with each other on wp.org (the same name is used for both a plugin and a theme). Those entries are tracked in `state/conflicts.json` and excluded from automatic migration. If you are the author of one side of a conflict and want your package published under the original name, open an issue.

## Documentation

Full architecture and operational documentation lives in [DOCS.md](./DOCS.md).
