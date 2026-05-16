package main

import (
	"log"

	"wpm-migration/pkg/store"
)

// recheck-closures prunes non-permanent entries from state/closed-themes.json
// and state/closed-plugins.json. The next `update` run will re-fetch every removed
// slug from wp.org and re-add it only if the closure is still in effect.
// Entries that wp.org has since reversed drop out of the list and become
// eligible for migration again.
//
// ClosurePermanent entries stay untouched: by definition those will never
// be reversed, so re-checking them daily is wasted API traffic.
func main() {
	closedThemes, err := store.GetClosedThemes()
	if err != nil {
		log.Printf("warning: failed to read closed themes json: %v", err)
	}
	closedPlugins, err := store.GetClosedPlugins()
	if err != nil {
		log.Printf("warning: failed to read closed plugins json: %v", err)
	}

	themesBefore := len(closedThemes)
	for slug, c := range closedThemes {
		if c != store.ClosurePermanent {
			delete(closedThemes, slug)
		}
	}

	pluginsBefore := len(closedPlugins)
	for slug, c := range closedPlugins {
		if c != store.ClosurePermanent {
			delete(closedPlugins, slug)
		}
	}

	if err := store.SetClosedThemes(closedThemes); err != nil {
		log.Fatalf("failed to write closed themes: %v", err)
	}
	if err := store.SetClosedPlugins(closedPlugins); err != nil {
		log.Fatalf("failed to write closed plugins: %v", err)
	}

	log.Printf("themes: kept %d permanent (removed %d non-permanent of %d total)",
		len(closedThemes), themesBefore-len(closedThemes), themesBefore)
	log.Printf("plugins: kept %d permanent (removed %d non-permanent of %d total)",
		len(closedPlugins), pluginsBefore-len(closedPlugins), pluginsBefore)
}
