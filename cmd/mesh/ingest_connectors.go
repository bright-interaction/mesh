package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/ingest"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

// ingestFlags are the knobs every connector subcommand shares.
type ingestFlags struct {
	vault string
	full  bool
	since string
	watch time.Duration
}

func bindIngestFlags(c *cobra.Command, f *ingestFlags) {
	c.Flags().StringVar(&f.vault, "vault", ".", "vault root")
	c.Flags().BoolVar(&f.full, "full", false, "ignore the saved high-water mark and re-pull everything")
	c.Flags().StringVar(&f.since, "since", "", "only pull items changed since this date (YYYY-MM-DD or RFC3339); overrides the saved mark")
	c.Flags().DurationVar(&f.watch, "watch", 0, "keep pulling on this interval (e.g. 15m, 1h); 0 = once and exit")
}

func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad --since %q (use YYYY-MM-DD or RFC3339)", s)
}

// reindexVault re-reads the vault and rebuilds the index so freshly imported notes
// are immediately searchable (mirrors `mesh index`).
func reindexVault(vaultRoot string) error {
	files, err := vault.Walk(vaultRoot)
	if err != nil {
		return err
	}
	notes, _ := index.ParseFiles(files, 0)
	for _, pn := range notes {
		if rel, err := filepath.Rel(vaultRoot, pn.Path); err == nil {
			pn.Path = rel
		}
	}
	g, _ := index.BuildGraph(notes)
	g.DetectCommunities(0)
	store, err := index.Open(vaultRoot)
	if err != nil {
		return err
	}
	defer store.Close()
	_, err = store.IndexVault(notes, g)
	return err
}

// watchLoop runs cycle once, then repeats every f.watch until interrupted (or just
// once when f.watch == 0). A cycle error aborts a one-shot run but only logs under
// --watch so a transient API failure does not kill a long-running schedule.
func watchLoop(f ingestFlags, cycle func(ctx context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		if err := cycle(ctx); err != nil {
			if f.watch <= 0 {
				return err
			}
			fmt.Fprintf(os.Stderr, "ingest: %v\n", err)
		}
		if f.watch <= 0 {
			return nil
		}
		fmt.Printf("next pull in %s (Ctrl-C to stop)\n", f.watch)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(f.watch):
		}
	}
}

// runConnector pulls one connector incrementally (honoring --full/--since on the
// first cycle), reindexes, and reschedules under --watch.
func runConnector(f ingestFlags, build func() (ingest.Connector, error)) error {
	since, err := parseSince(f.since)
	if err != nil {
		return err
	}
	first := true
	return watchLoop(f, func(ctx context.Context) error {
		conn, err := build()
		if err != nil {
			return err
		}
		opts := ingest.Opts{}
		if first {
			opts = ingest.Opts{Full: f.full, Since: since} // later watch cycles use the saved mark
		}
		first = false
		res, err := ingest.RunIncremental(ctx, f.vault, conn, opts)
		if err != nil {
			return err
		}
		fmt.Printf("ingested %d %s item(s) -> %s/ (%d written)\n", res.Pulled, conn.Name(), res.Folder, res.Written)
		if err := reindexVault(f.vault); err != nil {
			return err
		}
		fmt.Println("reindexed; imported notes are searchable")
		return nil
	})
}

func ingestGitHubCmd() *cobra.Command {
	var f ingestFlags
	var owner, repo string
	c := &cobra.Command{
		Use:   "github",
		Short: "Import GitHub issues + pull requests for a repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			if owner == "" || repo == "" {
				return fmt.Errorf("--owner and --repo are required")
			}
			return runConnector(f, func() (ingest.Connector, error) {
				return &ingest.GitHub{Owner: owner, Repo: repo, Token: os.Getenv("MESH_INGEST_GITHUB_TOKEN")}, nil
			})
		},
	}
	bindIngestFlags(c, &f)
	c.Flags().StringVar(&owner, "owner", "", "GitHub repo owner")
	c.Flags().StringVar(&repo, "repo", "", "GitHub repo name")
	return c
}

func ingestSlackCmd() *cobra.Command {
	var f ingestFlags
	var channel string
	c := &cobra.Command{
		Use:   "slack",
		Short: "Import a Slack channel's recent messages",
		RunE: func(cmd *cobra.Command, args []string) error {
			if channel == "" {
				return fmt.Errorf("--channel (a channel id) is required")
			}
			return runConnector(f, func() (ingest.Connector, error) {
				return &ingest.Slack{Channel: channel, Token: os.Getenv("MESH_INGEST_SLACK_TOKEN")}, nil
			})
		},
	}
	bindIngestFlags(c, &f)
	c.Flags().StringVar(&channel, "channel", "", "Slack channel id, e.g. C0123456")
	return c
}

func ingestLinearCmd() *cobra.Command {
	var f ingestFlags
	c := &cobra.Command{
		Use:   "linear",
		Short: "Import Linear issues (token in MESH_INGEST_LINEAR_TOKEN)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnector(f, func() (ingest.Connector, error) {
				return &ingest.Linear{Token: os.Getenv("MESH_INGEST_LINEAR_TOKEN")}, nil
			})
		},
	}
	bindIngestFlags(c, &f)
	return c
}

func ingestJiraCmd() *cobra.Command {
	var f ingestFlags
	var site, email, jql string
	c := &cobra.Command{
		Use:   "jira",
		Short: "Import Jira issues (token in MESH_INGEST_JIRA_TOKEN)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if site == "" {
				return fmt.Errorf("--site is required (e.g. https://acme.atlassian.net)")
			}
			return runConnector(f, func() (ingest.Connector, error) {
				return &ingest.Jira{Site: site, Email: email, Token: os.Getenv("MESH_INGEST_JIRA_TOKEN"), JQL: jql}, nil
			})
		},
	}
	bindIngestFlags(c, &f)
	c.Flags().StringVar(&site, "site", "", "Jira site URL, e.g. https://acme.atlassian.net")
	c.Flags().StringVar(&email, "email", "", "Atlassian account email (for basic auth with the API token)")
	c.Flags().StringVar(&jql, "jql", "", "optional base JQL filter (default: order by created DESC)")
	return c
}

func ingestNotionCmd() *cobra.Command {
	var f ingestFlags
	c := &cobra.Command{
		Use:   "notion",
		Short: "Import Notion pages (token in MESH_INGEST_NOTION_TOKEN)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnector(f, func() (ingest.Connector, error) {
				return &ingest.Notion{Token: os.Getenv("MESH_INGEST_NOTION_TOKEN")}, nil
			})
		},
	}
	bindIngestFlags(c, &f)
	return c
}

// ingestAllCmd runs every connector listed in .mesh/ingest.json, incrementally,
// then reindexes once. One cron line keeps the whole vault fed.
func ingestAllCmd() *cobra.Command {
	var f ingestFlags
	c := &cobra.Command{
		Use:   "all",
		Short: "Run every connector configured in .mesh/ingest.json (incremental)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := ingest.LoadConfig(f.vault)
			if err != nil {
				return err
			}
			if len(cfg.Connectors) == 0 {
				return fmt.Errorf("no connectors in %s/.mesh/ingest.json", f.vault)
			}
			since, err := parseSince(f.since)
			if err != nil {
				return err
			}
			first := true
			return watchLoop(f, func(ctx context.Context) error {
				total := 0
				for _, cc := range cfg.Connectors {
					conn, err := cc.Build()
					if err != nil {
						fmt.Fprintf(os.Stderr, "skip %s: %v\n", cc.Type, err)
						continue
					}
					opts := ingest.Opts{}
					if first {
						opts = ingest.Opts{Full: f.full, Since: since}
					}
					res, err := ingest.RunIncremental(ctx, f.vault, conn, opts)
					if err != nil {
						fmt.Fprintf(os.Stderr, "%s: %v\n", conn.Name(), err)
						continue
					}
					fmt.Printf("  %s: %d pulled, %d written\n", conn.Name(), res.Pulled, res.Written)
					total += res.Written
				}
				first = false
				if total > 0 {
					if err := reindexVault(f.vault); err != nil {
						return err
					}
					fmt.Printf("reindexed (%d new/updated notes)\n", total)
				} else {
					fmt.Println("nothing new")
				}
				return nil
			})
		},
	}
	bindIngestFlags(c, &f)
	return c
}
