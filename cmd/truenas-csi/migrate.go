package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/migration"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"k8s.io/client-go/kubernetes"
	kconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// runMigrate serves `truenas-csi migrate`: copy one PersistentVolumeClaim's
// data into another.
//
// It is the only administrative subcommand that writes, so it is deliberately
// awkward:
//
//   - it plans and prints, and does NOTHING else, unless -apply is given;
//   - -apply alone is not enough. The plan names the target, and -confirm must
//     repeat that name, so a command recalled from shell history cannot run
//     against a different claim than the one it was written for.
//
// Ownership is not re-checked here on purpose. internal/migration refuses any
// volume the driver does not own, by the same io.truenas.csi:managed property
// with source LOCAL that every destructive path in this driver checks. A second,
// looser check in the CLI would be a second answer to the same question.
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	var af adminFlags
	af.bind(fs)
	var (
		namespace = fs.String("namespace", "", "namespace of both claims (required)")
		source    = fs.String("source", "", "name of the PersistentVolumeClaim to copy FROM (required)")
		target    = fs.String("target", "", "name of the PersistentVolumeClaim to copy INTO (required)")
		mode      = fs.String("mode", string(migration.ModeAuto),
			"copy mode: auto, rsync or tar")
		image = fs.String("image", "", "image for the copy Job; empty uses the driver's default")
		sa    = fs.String("service-account", "", "ServiceAccount for the copy Job")
		apply = fs.Bool("apply", false,
			"actually run the copy. Without it this plans and prints, and changes nothing")
		confirm = fs.String("confirm", "",
			"repeat the target claim's name to authorise -apply. The plan prints it; a "+
				"command recalled from history cannot then run against a different claim")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *namespace == "" || *source == "" || *target == "" {
		return errors.New("migrate needs -namespace, -source and -target")
	}
	// The copy reads and writes whole volumes; the default 60s deadline is for
	// the read-only reports.
	if af.timeout == 60*time.Second {
		af.timeout = 6 * time.Hour
	}

	ctx, cancel := context.WithTimeout(context.Background(), af.timeout)
	defer cancel()

	cfg, err := config.Load(af.configPath)
	if err != nil {
		return fmt.Errorf("loading %s: %w", af.configPath, err)
	}
	name := af.backend
	if name == "" {
		if len(cfg.Backends) != 1 {
			return errors.New("-backend is required when several backends are configured")
		}
		for n := range cfg.Backends {
			name = n
		}
	}
	b, ok := cfg.Backends[name]
	if !ok {
		return fmt.Errorf("no configured backend named %q", name)
	}

	rc, err := kconfig.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes access: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}
	c, err := truenas.Dial(ctx, b)
	if err != nil {
		return fmt.Errorf("connecting to backend %q: %w", name, err)
	}
	defer func() { _ = c.Close() }()

	m := migration.New(cs, c, b)
	plan, err := m.Plan(ctx, migration.Request{
		SourceNamespace: *namespace,
		SourcePVC:       *source,
		TargetPVC:       *target,
		Mode:            migration.Mode(*mode),
		Image:           *image,
		ServiceAccount:  *sa,
	})
	if err != nil {
		return fmt.Errorf("planning the migration: %w", err)
	}

	if af.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(plan); err != nil {
			return err
		}
	} else {
		fmt.Printf("plan: copy %s/%s -> %s/%s on backend %q\n  mode: %s\n  job:  %s\n",
			*namespace, *source, *namespace, *target, name, plan.Mode, plan.JobName)
	}

	if !*apply {
		fmt.Printf("\nnothing was changed. Re-run with -apply -confirm %s to perform the copy.\n", *target)
		return nil
	}
	if *confirm != *target {
		return fmt.Errorf("-confirm must repeat the target claim's name (%q), got %q", *target, *confirm)
	}

	report, err := m.Run(ctx, plan)
	if err != nil {
		return fmt.Errorf("running the migration: %w", err)
	}
	if af.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Printf("copied %s/%s into %s\n", *namespace, *source, *target)
	return nil
}
