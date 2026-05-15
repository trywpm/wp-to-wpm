package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"wpm-migration/pkg/store"
	"wpm-migration/pkg/svn"

	"github.com/spf13/cobra"
)

type Options struct {
	registry      string
	migrationType string
	concurrency   int
}

func run(ctx context.Context, opts Options, packages []string) error {
	for _, pkg := range packages {
		println("Migrating package:", pkg)
	}

	// @todo: Implement the actual migration logic here, including:
	// - Fetching package details from the WordPress.org API
	// - Creating corresponding entries in the wpm registry
	// - Handling any necessary data transformations

	return nil
}

type svnLogEntry struct {
	Revision string `xml:"revision,attr"`
	Paths    []struct {
		Path string `xml:",chardata"`
	} `xml:"paths>path"`
}

func main() {
	var opts Options

	cmd := &cobra.Command{
		Use:           "migrate",
		Short:         "Migrate plugins and themes from wp.org to wpm",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			pkgType := store.PackageType(opts.migrationType)
			if !pkgType.Valid() {
				return fmt.Errorf("invalid migration type: %s", opts.migrationType)
			}

			rev, err := store.GetLastSvnRevision(pkgType)
			if err != nil {
				return fmt.Errorf("failed to get last SVN revision: %w", err)
			}

			var headRev int
			if len(args) == 0 {
				args, headRev, err = svn.GetUpdatedPackages(cmd.Context(), pkgType, rev+1)
				if err != nil {
					return fmt.Errorf("failed to get updated packages: %w", err)
				}
			}

			if err := run(cmd.Context(), opts, args); err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}

			if headRev > rev {
				if err := store.SetLastSvnRevision(store.PackageType(opts.migrationType), headRev); err != nil {
					return fmt.Errorf("failed to update last SVN revision: %w", err)
				}
			}

			return nil
		},
	}

	cmd.Flags().IntVarP(&opts.concurrency, "concurrency", "c", 5, "Number of concurrent migrations")

	cmd.Flags().StringVarP(&opts.registry, "registry", "r", "registry.wpm.so", "wpm registry url")
	cmd.Flags().StringVarP(&opts.migrationType, "type", "t", "", "Type of migration (plugin or theme)")

	if err := cmd.MarkFlagRequired("type"); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := cmd.ExecuteContext(ctx); err != nil {
		log.Fatal(err)
	}
}
