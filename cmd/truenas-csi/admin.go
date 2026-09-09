package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/pooladmin"
	"github.com/piwi3910/truenas-csi/internal/truenas"
)

// The administrative subcommands.
//
// They live in the driver's own binary rather than a second one because the
// release pipeline builds, signs and attests exactly one image; a second binary
// would need its own build, signature and SBOM to be trustworthy, and would
// still need the same credentials.
//
// They are dispatched before flag.Parse so the driver's own flag set is
// untouched: the DaemonSet and Deployment pass exactly what they always did,
// and `truenas-csi -mode=node ...` cannot be shadowed by a subcommand name
// because every subcommand is a bare word and every driver argument begins with
// a dash.
const (
	cmdPool    = "pool"
	cmdMigrate = "migrate"
)

// dispatchSubcommand runs an administrative subcommand and reports whether it
// handled the invocation. The driver runs normally when it returns false.
func dispatchSubcommand(args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case cmdPool:
		return true, runPool(args[1:])
	case cmdMigrate:
		return true, runMigrate(args[1:])
	}
	return false, nil
}

// subcommandHelp is appended to the driver's own usage.
func subcommandHelp() string {
	return "\nAdministrative subcommands:\n" +
		"  pool status|disks|alerts   report pool health, disk health and appliance alerts\n" +
		"  migrate                    copy one PersistentVolumeClaim's data into another\n" +
		"\nRun `truenas-csi <subcommand> -h` for their flags.\n"
}

// adminFlags are the arguments every administrative subcommand shares.
type adminFlags struct {
	configPath string
	backend    string
	asJSON     bool
	timeout    time.Duration
}

func (a *adminFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&a.configPath, "config", "/etc/truenas-csi/config.yaml",
		"path to the driver's configuration. The same file the driver reads, so an "+
			"administrative command cannot be pointed at credentials the driver does not use")
	fs.StringVar(&a.backend, "backend", "",
		"configured backend name; empty means every configured backend")
	fs.BoolVar(&a.asJSON, "json", false, "emit JSON instead of a table")
	fs.DurationVar(&a.timeout, "timeout", 60*time.Second, "overall deadline")
}

// appliances dials the backends the flags select.
//
// Credentials come from the driver's config file and never from an argument:
// an API key in argv is readable through /proc by every process on the host,
// and this binary is frequently run on a node.
func (a *adminFlags) appliances(ctx context.Context) ([]pooladmin.Backend, func(), error) {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading %s: %w", a.configPath, err)
	}
	var out []pooladmin.Backend
	var clients []*truenas.Client
	closeAll := func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}
	for name, b := range cfg.Backends {
		if a.backend != "" && name != a.backend {
			continue
		}
		c, err := truenas.Dial(ctx, b)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("connecting to backend %q: %w", name, err)
		}
		clients = append(clients, c)
		out = append(out, pooladmin.Backend{Name: name, Client: c})
	}
	if len(out) == 0 {
		closeAll()
		return nil, nil, fmt.Errorf("no configured backend matches %q", a.backend)
	}
	return out, closeAll, nil
}

// runPool serves `truenas-csi pool status|disks|alerts`.
//
// Every call it makes is a read. internal/pooladmin enforces that with a closed
// set of allowed trailing method components, so this command cannot be talked
// into scrubbing, replacing or dismissing anything.
func runPool(args []string) error {
	if len(args) == 0 {
		return errors.New("pool needs a subcommand: status, disks or alerts")
	}
	sub := args[0]
	fs := flag.NewFlagSet("pool "+sub, flag.ContinueOnError)
	var af adminFlags
	af.bind(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), af.timeout)
	defer cancel()
	backends, closeAll, err := af.appliances(ctx)
	if err != nil {
		return err
	}
	defer closeAll()

	switch sub {
	case "status":
		return emit(af, os.Stdout, backends, func(b pooladmin.Backend) (any, error) {
			return pooladmin.PoolStatus(ctx, b)
		}, poolTable)
	case "disks":
		return emit(af, os.Stdout, backends, func(b pooladmin.Backend) (any, error) {
			return pooladmin.DiskHealth(ctx, b)
		}, diskTable)
	case "alerts":
		return emit(af, os.Stdout, backends, func(b pooladmin.Backend) (any, error) {
			return pooladmin.Alerts(ctx, b)
		}, alertTable)
	}
	return fmt.Errorf("unknown pool subcommand %q: want status, disks or alerts", sub)
}

// emit renders one report per backend, as JSON or as a table.
func emit(af adminFlags, w io.Writer, backends []pooladmin.Backend,
	collect func(pooladmin.Backend) (any, error),
	table func(*tabwriter.Writer, string, any),
) error {
	results := make(map[string]any, len(backends))
	for _, b := range backends {
		v, err := collect(b)
		if err != nil {
			return fmt.Errorf("backend %q: %w", b.Name, err)
		}
		results[b.Name] = v
	}
	if af.asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, b := range backends {
		table(tw, b.Name, results[b.Name])
	}
	return tw.Flush()
}

// humanBytes renders a byte count the way an administrator reads one.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func poolTable(w *tabwriter.Writer, backend string, v any) {
	pools, _ := v.([]pooladmin.Pool)
	_, _ = fmt.Fprintf(w, "\n%s — pools\n", backend)
	_, _ = fmt.Fprintln(w, "NAME\tSTATUS\tHEALTHY\tSIZE\tFREE\tUSED%\tFRAG%\tSCRUB")
	for _, p := range pools {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\t%.0f\t%.0f\t%s\n",
			p.Name, p.Status, p.Healthy, humanBytes(p.SizeBytes), humanBytes(p.FreeBytes),
			p.UsedPercent, p.FragmentationPercent, p.Scrub.State)
	}
}

func diskTable(w *tabwriter.Writer, backend string, v any) {
	disks, _ := v.([]pooladmin.Disk)
	_, _ = fmt.Fprintf(w, "\n%s — disks\n", backend)
	_, _ = fmt.Fprintln(w, "NAME\tPOOL\tSIZE\tMODEL\tSERIAL\tSMART")
	for _, d := range disks {
		// "unavailable" and "disabled" are different claims about someone's
		// hardware. TrueNAS 25.10 exposes no SMART API at all, and printing
		// "disabled" for every disk on every such appliance was simply wrong.
		smart := d.SMARTStatus
		if !d.SMARTAvailable {
			smart = "unavailable"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			d.Name, d.Pool, humanBytes(d.SizeBytes), d.Model, d.Serial, smart)
	}
}

func alertTable(w *tabwriter.Writer, backend string, v any) {
	alerts, _ := v.([]pooladmin.Alert)
	_, _ = fmt.Fprintf(w, "\n%s — alerts\n", backend)
	if len(alerts) == 0 {
		_, _ = fmt.Fprintln(w, "(none)")
		return
	}
	_, _ = fmt.Fprintln(w, "LEVEL\tCLASS\tWHEN\tMESSAGE")
	for _, a := range alerts {
		if a.Dismissed {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			a.Level, a.Class, a.Time.Format(time.RFC3339), a.Formatted)
	}
}
