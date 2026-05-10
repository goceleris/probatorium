package fault

import (
	"context"
	"fmt"
)

// TCNetem injects netem-shaped network perturbation on iface of host.
// Delay / jitter / loss / reorder are configured per instance; the
// Apply call installs a single qdisc replacing any pre-existing root,
// and Undo deletes it.
//
// The fault is conservative — it ALWAYS replaces (not adds) the root
// qdisc, so a stale fault from a crashed prior run cannot stack
// behaviours.
type TCNetem struct {
	// Host is the target host. tc-netem faults usually land on
	// HostClient so they perturb the path into celeris.
	Host Host
	// Iface is the device, e.g. "eth0" / "ens5".
	Iface string
	// Delay is the mean added latency. Zero disables delay.
	DelayMs int
	// JitterMs adds a uniform jitter window. Ignored if DelayMs is 0.
	JitterMs int
	// LossPct is packet loss in [0, 100].
	LossPct float64
	// CorruptPct is packet corruption in [0, 100].
	CorruptPct float64
	// ReorderPct is packet reorder probability in [0, 100].
	ReorderPct float64
}

// String returns a stable identifier suitable for incident logs.
func (f *TCNetem) String() string {
	return fmt.Sprintf("tc-netem(host=%s iface=%s delay=%dms±%d loss=%.1f%% corrupt=%.1f%% reorder=%.1f%%)",
		f.Host, f.Iface, f.DelayMs, f.JitterMs, f.LossPct, f.CorruptPct, f.ReorderPct)
}

// Apply runs `tc qdisc replace dev <iface> root netem ...`. Replace
// (not add) so a stale qdisc from a crashed prior run is cleared.
func (f *TCNetem) Apply(ctx context.Context) error {
	argv := []string{"tc", "qdisc", "replace", "dev", f.Iface, "root", "netem"}
	if f.DelayMs > 0 {
		argv = append(argv, "delay", fmt.Sprintf("%dms", f.DelayMs))
		if f.JitterMs > 0 {
			argv = append(argv, fmt.Sprintf("%dms", f.JitterMs))
		}
	}
	if f.LossPct > 0 {
		argv = append(argv, "loss", fmt.Sprintf("%.2f%%", f.LossPct))
	}
	if f.CorruptPct > 0 {
		argv = append(argv, "corrupt", fmt.Sprintf("%.2f%%", f.CorruptPct))
	}
	if f.ReorderPct > 0 {
		argv = append(argv, "reorder", fmt.Sprintf("%.2f%%", f.ReorderPct))
	}
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("tc-netem.apply", argv, out, err)
	}
	return nil
}

// Undo runs `tc qdisc del dev <iface> root`. Errors are returned so
// the scheduler can log them; the scheduler does not abort on Undo
// failure.
func (f *TCNetem) Undo(ctx context.Context) error {
	argv := []string{"tc", "qdisc", "del", "dev", f.Iface, "root"}
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("tc-netem.undo", argv, out, err)
	}
	return nil
}
