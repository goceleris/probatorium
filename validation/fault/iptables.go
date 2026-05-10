package fault

import (
	"context"
	"fmt"
	"strconv"
)

// IPTablesDrop inserts an iptables DROP rule on Apply and removes it
// on Undo. Used to simulate transient network partitions while the
// rest of the path remains healthy (only celeris's bind port is
// disrupted, not the host).
type IPTablesDrop struct {
	// Host is the host whose tables we mutate. Typically HostServer.
	Host Host
	// Chain is INPUT or OUTPUT depending on which direction the test
	// wants disrupted. INPUT drops loadgen->celeris, OUTPUT drops
	// celeris->loadgen.
	Chain string
	// Port is the TCP port to block.
	Port int
	// Proto is "tcp" or "udp"; defaults to "tcp" if empty.
	Proto string
}

func (f *IPTablesDrop) proto() string {
	if f.Proto == "" {
		return "tcp"
	}
	return f.Proto
}

func (f *IPTablesDrop) String() string {
	return fmt.Sprintf("iptables-drop(host=%s chain=%s port=%d proto=%s)",
		f.Host, f.Chain, f.Port, f.proto())
}

// rule returns the rule fragment used by both -I (apply) and -D (undo)
// so the two operations point at the exact same match spec.
func (f *IPTablesDrop) rule() []string {
	return []string{
		f.Chain,
		"-p", f.proto(),
		"--dport", strconv.Itoa(f.Port),
		"-j", "DROP",
	}
}

func (f *IPTablesDrop) Apply(ctx context.Context) error {
	if f.Chain != "INPUT" && f.Chain != "OUTPUT" {
		return fmt.Errorf("iptables-drop: bad chain %q (want INPUT|OUTPUT)", f.Chain)
	}
	if f.Port <= 0 {
		return fmt.Errorf("iptables-drop: bad port %d", f.Port)
	}
	argv := append([]string{"iptables", "-I"}, f.rule()...)
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("iptables-drop.apply", argv, out, err)
	}
	return nil
}

func (f *IPTablesDrop) Undo(ctx context.Context) error {
	argv := append([]string{"iptables", "-D"}, f.rule()...)
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("iptables-drop.undo", argv, out, err)
	}
	return nil
}
