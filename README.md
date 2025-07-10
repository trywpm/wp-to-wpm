# wp to wpm

This repository contains information and scripts to migrate WordPress plugins and themes from the official WordPress.org SVN repository to wpm (package manager and registry for wp ecosystem).

## 📋 Overview

wpm is a modern package manager for WordPress that provides an alternative distribution method for WordPress plugins and themes. This repository serves as the central hub for tracking which WordPress.org packages have been migrated to wpm and managing the migration process.

## 🗂️ Repository Structure

- **`themes.json`** - Contains all theme names that have been successfully migrated from WordPress.org SVN to WPM
- **`plugins.json`** - Contains all plugin names that have been successfully migrated from WordPress.org SVN to WPM  
- **`conflicts.json`** - Lists themes and plugins with conflicting names that cannot be automatically published (authors can manually request publication under the original name)

## 🚀 How to Request Migration

### For Plugin/Theme Authors

If your WordPress plugin or theme exists on WordPress.org but hasn't been migrated to WPM yet, you can request migration by:

1. **Fork this repository**
2. **Create a new branch** for your request
3. **Add your plugin/theme name** to the appropriate JSON file:
   - Add to `plugins.json` for plugins
   - Add to `themes.json` for themes
4. **Submit a Pull Request** with:
   - Clear title indicating the plugin/theme name
   - Description explaining why you want it migrated

### For Community Members

Community members can also submit migration requests for popular plugins/themes that aren't yet available on wpm by following the same process above.

## 🛑 Removal Requests

If you are the author of a plugin or theme that has been migrated to wpm and you want it removed:

1. **Open an Issue** in this repository
2. **Explain the reason** for removal (optional but helpful)
3. A maintainer will review and process the removal request

## 📝 Migration Guidelines

### Automatic Migration Criteria
- Plugin/theme must exist on WordPress.org
- Must have a clean history (no security issues)
- No naming conflicts with existing wpm packages

### Name Conflicts
When a plugin or theme name conflicts with an existing WPM package:
- The item is added to `conflicts.json`
- Original authors can request manual review for name reservation
- Alternative naming may be suggested

## 🔧 For Maintainers

### Processing Migration Requests
1. Verify the plugin/theme exists on WordPress.org
2. Check for naming conflicts
3. Run migration scripts (if available)
4. Update appropriate JSON file
5. Merge the pull request

### Processing Removal Requests
1. Verify ownership claims
2. Remove from wpm registry
3. Update JSON files
4. Close the issue with confirmation

## 📊 Statistics

- **Themes migrated**: ~10000+ themes
- **Plugins migrated**: ~35000+ plugins
- **Conflicts identified**: Check `conflicts.json` for current count

## 🤝 Contributing

We welcome contributions from the WordPress community! Whether you're:
- Requesting migration of your own packages
- Helping migrate popular community packages  
- Improving documentation
- Reporting issues
