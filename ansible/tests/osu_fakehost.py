"""A fake cluster host for the tests of ansible/files/os_upgrade.py (and, through the wrapper, of
ansible/os-upgrade.yml): a directory tree standing in for /proc, /sys, /boot, /etc, /lib/modules ..., and ONE stub
program, installed under the name of every command the script runs, that answers from a JSON state file, logs its
argv, and applies the few state changes a real command would (grub-set-default, grub-reboot, dkms autoinstall,
apt-get full-upgrade). Python standard library only.
"""
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "..", "files", "os_upgrade.py")
FIXTURES = os.path.join(HERE, "fixtures")

COMMANDS = ["dpkg-query", "dpkg", "apt-get", "apt-mark", "apt-config", "apt-cache", "modinfo", "dkms", "grub-editenv",
            "grub-set-default", "grub-reboot", "update-grub", "update-initramfs", "findmnt", "df", "timedatectl", "systemctl",
            "sysctl", "cc", "curl", "mokutil", "gcc", "make", "ip", "ping", "sshd", "tailscaled", "tailscale", "snap", "netplan"]

STUB = r'''#!__PYTHON__
import fnmatch, json, os, sys, time, shutil
name = os.path.basename(sys.argv[0])
args = sys.argv[1:]
state_dir = os.environ["STUB_STATE"]
root = os.environ.get("OSU_ROOT", "")
sp = os.path.join(state_dir, "state.json")
st = json.load(open(sp))
def save():
    json.dump(st, open(sp, "w"))
with open(os.environ["STUB_LOG"], "a") as f:
    f.write(name + " " + " ".join(args) + "\n")
def out(s):
    sys.stdout.write(s)
def pkgs():
    return st["packages"]
def regen():
    nxt = st.get("next_grub_cfg")
    if nxt:
        if os.path.abspath(nxt) != os.path.abspath(root + "/boot/grub/grub.cfg"):
            shutil.copy(nxt, root + "/boot/grub/grub.cfg")
        return st.get("update_grub_rc", 0)
    if st.get("grub_template"):
        import re
        tpl = open(st["grub_template"]).read()
        simple_at = tpl.index("menuentry 'Ubuntu' --class")
        sub_at = tpl.index("submenu 'Advanced options")
        sub_end = tpl.index("\n}\n", sub_at) + 3
        body = tpl[tpl.index("\n", sub_at) + 1:sub_end - 2]
        normal, recovery = re.findall(r"\tmenuentry .*?\n\t}\n", body, re.S)[:2]
        k0 = re.search(r"vmlinuz-(\S+)", normal).group(1)
        def key(v):
            m = re.match(r"(\d+)\.(\d+)\.(\d+)-(\d+)", v)
            return tuple(int(x) for x in m.groups())
        ks = sorted((os.path.basename(x)[8:] for x in __import__("glob").glob(root + "/boot/vmlinuz-*")), key=key, reverse=True)
        simple = tpl[simple_at:sub_at].replace(k0, ks[0])
        head = tpl[:simple_at]
        if not os.path.exists(root + "/etc/default/grub.d/99-celeris-os-upgrade.cfg"):
            a = head.index('if [ "${next_entry}" ] ; then')
            b = head.index("fi\n", head.index('set default="${saved_entry}"')) + 3
            head = head[:a] + 'set default="0"\n' + head[b:]
        sub = tpl[sub_at:tpl.index("\n", sub_at) + 1] + "".join(normal.replace(k0, k) + recovery.replace(k0, k) for k in ks) + "}\n"
        open(root + "/boot/grub/grub.cfg", "w").write(head + simple + sub + tpl[sub_end:])
    return st.get("update_grub_rc", 0)
if name == "dpkg-query":
    fmt = "${Package}\n"
    pats = []
    i = 0
    while i < len(args):
        if args[i] == "-f":
            fmt = args[i + 1].encode().decode("unicode_escape"); i += 2
        elif args[i] == "-W":
            i += 1
        else:
            pats.append(args[i]); i += 1
    found = False
    for n in sorted(pkgs()):
        if pats and not any(fnmatch.fnmatch(n, p) for p in pats):
            continue
        p = pkgs()[n]
        found = True
        out(fmt.replace("${Package}", n).replace("${Version}", p["version"]).replace("${Status}", "install ok " + p.get("status", "installed"))
            .replace("${Architecture}", "arm64").replace("${db:Status-Abbrev}", "ii "))
    sys.exit(0 if found or not pats else 1)
if name == "dpkg":
    if args[:1] == ["-l"]:
        out("Desired=Unknown/Install/Remove/Purge/Hold\n" + "".join("ii  %s  %s  arm64  test package\n" % (n, pkgs()[n]["version"]) for n in sorted(pkgs())) ); sys.exit(0)
    if "--configure" in args:
        if st.get("configure_ok") or not st.get("audit"):
            st["audit"] = []
            for n in pkgs():
                if pkgs()[n].get("status") != "installed":
                    pkgs()[n]["status"] = "installed"
            save(); sys.exit(0)
        sys.stderr.write("dpkg: error processing package linux-headers (--configure): dkms build failed\n"); sys.exit(1)
    if args[:1] == ["--audit"]:
        out("\n".join(st.get("audit", [])) + ("\n" if st.get("audit") else "")); sys.exit(0)
    if args[:1] == ["-S"]:
        if st.get("src_pkg"):
            out(st["src_pkg"] + ": " + args[1] + "\n"); sys.exit(0)
        sys.exit(1)
if name == "apt-get":
    if "-s" in args and "remove" in args:
        names = args[args.index("--purge") + 1:] if "--purge" in args else []
        out("".join("Remv %s [1]\n" % n for n in names + st.get("remove_cascade", [])))
        sys.exit(st.get("sim_rc", 0))
    if "-s" in args:
        out("\n".join(st.get("sim_phased" if any("Always-Include-Phased" in a for a in args) else "sim", [])) + "\n"); sys.exit(st.get("sim_rc", 0))
    if "update" in args:
        out("Hit:1 http://ports.ubuntu.com resolute InRelease\nReading package lists...\n"); sys.exit(st.get("update_rc", 0))
    if "remove" in args:
        names = (args[args.index("--purge") + 1:] if "--purge" in args else []) + st.get("remove_cascade", [])
        out("Removing: %s\n" % " ".join(names))
        if st.get("remove_noop"):
            sys.exit(0)
        if st.get("remove_rc"):
            sys.stderr.write("E: Sub-process /usr/bin/dpkg returned an error code (1)\n"); sys.exit(st["remove_rc"])
        for n in names:
            pkgs().pop(n, None)
            if n.startswith("linux-image-"):
                k = n[len("linux-image-"):]
                for pth in ("/boot/vmlinuz-" + k, "/boot/initrd.img-" + k):
                    try:
                        os.remove(root + pth)
                    except OSError:
                        pass
        if st.get("abort_clears_audit", True):
            st["audit"] = []
        if st.get("grub_template"):
            regen()
        save(); sys.exit(st.get("remove_rc_after", 0))  # remove_rc_after: the packages are gone and apt still reports an error
    if "full-upgrade" in args:
        out("policy-rc.d present: %s\n" % os.path.exists(root + "/usr/sbin/policy-rc.d"))
        out("held: %s\n" % " ".join(st.get("held", [])))
        out("env: %s %s %s\n" % (os.environ.get("DEBIAN_FRONTEND"), os.environ.get("NEEDRESTART_MODE"), os.environ.get("UCF_FORCE_CONFFOLD")))
        out("args: %s\n" % " ".join(args))
        time.sleep(float(st.get("upgrade_sleep", 0.3)))
        for n, v in st.get("upgrade_adds", {}).items():
            pkgs()[n] = {"version": v, "status": "installed"}
        if st.get("upgrade_audit"):
            st["audit"] = st["upgrade_audit"]
        for k in st.get("upgrade_kernels", []):
            os.makedirs(root + "/boot", exist_ok=True)
            open(root + "/boot/vmlinuz-" + k, "w").close()
            open(root + "/boot/initrd.img-" + k, "w").close()
            os.makedirs(root + "/lib/modules/" + k + "/build", exist_ok=True)
            open(root + "/lib/modules/" + k + "/build/Makefile", "w").close()
            open(root + "/lib/modules/" + k + "/modules.alias", "w").write(
                ("alias pci:v000010ECd00008127sv*sd*bc*sc*i* r8169\n" if st.get("new_kernel_alias_races") else "") + "alias pci:v000010ECd00008127sv*sd*bc*sc*i* r8127\n")
            st["modinfo"][k + "/r8169"] = {"filename": "/lib/modules/" + k + "/kernel/r8169.ko.zst"}
            pkgs()["linux-modules-" + k] = {"version": k, "status": "installed"}
            pkgs()["linux-headers-" + k] = {"version": k, "status": "half-configured" if st.get("upgrade_audit") else "installed"}
            pkgs()["linux-image-" + k] = {"version": k, "status": "installed"}
        if st.get("upgrade_kernels"):
            regen()
        if st.get("unhold_rc_after_upgrade"):  # unattended-upgrades takes the dpkg frontend lock while the detached run ends
            st["unhold_rc"] = st["unhold_rc_after_upgrade"]
        save()
        if st.get("upgrade_dies"):
            os.kill(os.getppid(), 9)  # the detached run is killed mid-apt (OOM, power): no rc file, no policy-rc.d removal
            time.sleep(1)
        sys.exit(st.get("upgrade_rc", 0))
if name == "apt-mark":
    if args[:1] == ["showhold"]:
        if st.get("showhold_rc"):
            sys.stderr.write("E: Could not open file /var/lib/dpkg/status (test)\n"); sys.exit(st["showhold_rc"])
        out("\n".join(st.get("held", [])) + "\n"); sys.exit(0)
    if args[:1] == ["hold"]:
        st["held"] = sorted(set(st.get("held", [])) | set(args[1:])); save(); sys.exit(0)
    if args[:1] == ["unhold"]:
        if st.get("unhold_rc"):  # the dpkg frontend lock is taken (unattended-upgrades): fails and changes nothing
            sys.stderr.write("E: Could not get lock /var/lib/dpkg/lock-frontend (test)\n"); sys.exit(st["unhold_rc"])
        # unhold_keeps: names that stay held although apt-mark says it worked
        st["held"] = [h for h in st.get("held", []) if h not in args[1:] or h in st.get("unhold_keeps", [])]; save(); sys.exit(0)
    sys.exit(st.get("apt_mark_rc", 0))
if name == "apt-config":
    out("\n".join('%s "%s";' % kv for kv in st.get("apt_config", {}).items()) + "\n"); sys.exit(0)
if name == "apt-cache":
    out("Package files:\n 100 /var/lib/dpkg/status\n"); sys.exit(0)
if name == "modinfo":
    k = args[args.index("-k") + 1]; m = args[-1]
    d = st["modinfo"].get(k + "/" + m)
    if not d:
        sys.stderr.write("modinfo: ERROR: Module %s not found.\n" % m); sys.exit(1)
    if "-F" not in args:
        sys.exit(0)
    f = args[args.index("-F") + 1]
    v = d.get(f, "")
    if f == "alias":
        v = "pci:v000010ECd00008127sv*sd*bc*sc*i*" if (d.get("filename") and not d.get("no_alias")) else ""
    out(v + "\n"); sys.exit(0)
if name == "dkms":
    if args[:1] == ["status"]:
        out("\n".join(st.get("dkms_status", [])) + "\n"); sys.exit(0)
    if args[:1] == ["autoinstall"]:
        k = args[args.index("-k") + 1]
        out("Building module r8127 for %s\n" % k)
        if st.get("dkms_build_ok", True):
            st["dkms_status"].append("r8127/11.015.00, %s, aarch64: installed" % k)
            st["modinfo"]["%s/r8127" % k] = {"filename": "/lib/modules/%s/updates/dkms/r8127.ko.zst" % k, "version": "11.015.00", "signer": st.get("signer", "")}
            os.makedirs(root + "/lib/modules/%s/build" % k, exist_ok=True)
            save(); sys.exit(0)
        save(); sys.stderr.write("Error! Bad return status for module build\n"); sys.exit(10)
if name == "grub-editenv":
    if args[:2] == ["-", "unset"]:
        if st.get("unset_rc"):
            sys.stderr.write("grub-editenv: cannot write the environment block (test)\n"); sys.exit(st["unset_rc"])
        for k in args[2:]:
            st["grubenv"][k] = ""
        save(); sys.exit(0)
    if args[-1:] == ["list"]:
        out("".join("%s=%s\n" % kv for kv in st["grubenv"].items() if kv[1] != "")); sys.exit(0)
if name == "grub-set-default":
    st["grubenv"]["saved_entry"] = args[0]; save(); sys.exit(st.get("set_default_rc", 0))
if name == "grub-reboot":
    if st.get("reboot_noop"):
        sys.exit(0)  # reports success and writes nothing
    st["grubenv"]["next_entry"] = "wrong-entry" if st.get("reboot_sets_wrong") else args[0]; save(); sys.exit(0)
if name == "update-grub":
    sys.exit(regen())
if name == "update-initramfs":
    if "-k" in args and os.path.exists(root + "/boot/initrd.img-" + args[args.index("-k") + 1]):
        os.utime(root + "/boot/initrd.img-" + args[args.index("-k") + 1], None)  # rebuilt now
    sys.exit(st.get("initramfs_rc", 0))
if name == "findmnt":
    out((st["boot_fs"] if "FSTYPE" in args else st.get("boot_uuid", "") if "UUID" in args else st["boot_src"]) + "\n"); sys.exit(0)
if name == "df":
    path = args[-1][len(root):] or "/"
    best = None
    for mnt in st["df"]:
        if (path == mnt or path.startswith(mnt.rstrip("/") + "/") or mnt == "/") and (best is None or len(mnt) > len(best)):
            best = mnt
    d = st["df"].get(best)
    if not d:
        sys.exit(1)
    out("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x %d %d %d 50%% %s\n" % (d["total"], d["total"] - d["free"], d["free"], best)); sys.exit(0)
if name == "timedatectl":
    out(st.get("ntp", "yes") + "\n"); sys.exit(0)
if name == "systemctl":
    if args[0] == "--failed":
        sys.exit(0)
    nth = st.get("systemctl_kill_nth", {}).get(" ".join(args[:2]))
    if nth:  # killed at the Nth call of this very command (a read too): "is-active apt-daily.timer" 2 is the check after the freeze's changes
        cnt = st.setdefault("systemctl_counts", {})
        cnt[" ".join(args[:2])] = cnt.get(" ".join(args[:2]), 0) + 1
        save()
        if cnt[" ".join(args[:2])] == nth:
            os.kill(os.getppid(), 9)
            time.sleep(1)
            sys.exit(1)
    if args[0] == "is-system-running":
        out("running\n"); sys.exit(0)
    if args[0] in ("stop", "disable", "start", "enable"):
        unit = args[-1]  # "enable --runtime U": the unit is the last word
        key = {"stop": ("is-active", "inactive"), "start": ("is-active", "active"), "disable": ("is-enabled", "disabled"),
               "enable": ("is-enabled", "enabled-runtime" if "--runtime" in args else "enabled")}[args[0]]
        if args[0] + " " + unit in st.get("systemctl_fail", []):
            sys.stderr.write("Failed to %s %s: Unit is busy (test)\n" % (args[0], unit)); sys.exit(1)
        if args[0] + " " + unit in st.get("systemctl_kill", []):
            os.kill(os.getppid(), 9)  # the freeze is killed here (a runner lost, a power loss): the verb is not applied
            time.sleep(1)
            sys.exit(1)
        if args[0] + " " + unit in st.get("systemctl_noop", []):
            sys.exit(0)  # reports success and changes nothing
        if st.get("services", {}).get(key[0] + " " + unit) == "masked" and args[0] == "enable":
            sys.stderr.write("Failed to enable unit: Unit %s is masked (test)\n" % unit); sys.exit(1)
        st["services"][key[0] + " " + unit] = key[1]; save()
        if args[0] + " " + unit in st.get("systemctl_kill_after", []):
            os.kill(os.getppid(), 9)  # the verb WAS applied and the process dies before it can record it
            time.sleep(1)
            sys.exit(1)
        sys.exit(0)
    out(st.get("services", {}).get(args[0] + " " + args[1], "inactive" if args[0] == "is-active" else "disabled") + "\n"); sys.exit(0)
if name == "ip":
    if st.get("addr", True):
        out("".join("2: bond0    inet %s/24 brd 192.168.50.255 scope global bond0\n" % ip for ip in ("192.168.50.65", "192.168.50.199", "192.168.50.195")))
    sys.exit(0)
if name == "ping":
    sys.exit(0 if st.get("ping", True) else 1)
if name == "sysctl":
    out("kernel.pid_max = 4194304\nvm.swappiness = 60\n"); sys.exit(0)
if name in ("cc", "gcc"):
    if "-o" in args:
        exe = args[args.index("-o") + 1]
        open(exe, "w").write("#!/bin/sh\necho ok\n"); os.chmod(exe, 0o755)
    sys.exit(st.get("cc_rc", 0))
if name == "curl":
    sys.exit(st.get("curl_rc", {}).get(args[-1].replace("https://", ""), 0))
if name == "sshd":
    sys.exit(st.get("sshd_rc", 0))
if name == "tailscale":
    if args[:2] == ["debug", "prefs"]:
        if st.get("tailscale_prefs_rc"):
            sys.exit(st["tailscale_prefs_rc"])
        out(json.dumps({"AutoUpdate": st.get("tailscale_autoupdate", {"Check": True, "Apply": False})}) + "\n"); sys.exit(0)
    out("1.90.0\n"); sys.exit(0)
if name == "snap":
    if args[:2] == ["refresh", "--time"]:
        if st.get("snap_refresh_rc"):
            sys.exit(st["snap_refresh_rc"])
        out("timer: 00:00~24:00/4\nlast: today at 06:12 CEST\nnext: today at 18:30 CEST\n"); sys.exit(0)
    sys.exit(1)
if name == "tailscaled":
    out("1.90.0\n"); sys.exit(st.get("tailscaled_rc", 0))
if name == "netplan":
    sys.stderr.write("netplan: invalid configuration\n") if st.get("netplan_rc", 0) else None
    sys.exit(st.get("netplan_rc", 0))
if name == "mokutil":
    out("SecureBoot %s\n" % st.get("sb", "disabled")); sys.exit(0)
if name == "make":
    sys.exit(0)
sys.stderr.write("stub: no behaviour for %s %s\n" % (name, args)); sys.exit(99)
'''




def _wf(path, content):
    with open(path, "w") as f:
        f.write(content)

class FakeHost:
    """work/root (the tree), work/bin (the stubs), work/state (state.json and the argv log)."""

    def __init__(self, work, hostname="msr1", arm64=True):
        self.work = work
        self.root = os.path.join(work, "root")
        self.bin = os.path.join(work, "bin")
        self.state_dir = os.path.join(work, "state")
        self.log = os.path.join(self.state_dir, "argv.log")
        for d in (self.root, self.bin, self.state_dir):
            os.makedirs(d, exist_ok=True)
        _wf(self.log, "")
        stub = os.path.join(self.bin, "_stub")
        _wf(stub, STUB.replace("__PYTHON__", sys.executable))
        os.chmod(stub, 0o755)
        for c in COMMANDS + ["grub-probe"]:
            os.symlink("_stub", os.path.join(self.bin, c))
        self.grub_cfg_fixture = os.path.join(FIXTURES, "grub-arm64-efi.cfg" if arm64 else "grub-amd64.cfg")
        self.kernels = ["7.0.0-38-generic", "7.0.0-31-generic"] if arm64 else ["6.14.0-44-generic", "6.14.0-37-generic"]
        self.running = self.kernels[1]
        self.uuid = "0b1c2d3e-aaaa-bbbb-cccc-000000000001" if arm64 else "9f8e7d6c-1111-2222-3333-000000000002"
        self.state = {}
        self.build(hostname, arm64)

    # ---- the tree
    def w(self, path, content="", mode=None):
        full = self.root + path
        os.makedirs(os.path.dirname(full), exist_ok=True)
        with open(full, "w") as f:
            f.write(content)
        if mode:
            os.chmod(full, mode)

    def proc(self, pid, *argv, comm=None):
        """A running process: /proc/<pid>/cmdline (NUL separated, as the kernel prints it) and /proc/<pid>/comm (the first 15
        characters of the program or script name, as the kernel keeps them)."""
        self.w("/proc/%d/cmdline" % pid)
        with open("%s/proc/%d/cmdline" % (self.root, pid), "wb") as f:
            f.write(b"".join(a.encode() + b"\0" for a in argv))
        base = os.path.basename(argv[1] if len(argv) > 1 and os.path.basename(argv[0]).startswith(("python", "perl", "sh", "bash", "dash")) else argv[0])
        self.w("/proc/%d/comm" % pid, (comm if comm is not None else base[:15]) + "\n")

    def build(self, hostname, arm64):
        self.w("/etc/hostname", hostname + "\n")
        self.w("/etc/os-release", 'PRETTY_NAME="Ubuntu 26.04 LTS"\nNAME="Ubuntu"\nVERSION_ID="26.04"\nID=ubuntu\nVERSION_CODENAME=resolute\n')
        self.w("/proc/sys/kernel/osrelease", self.running + "\n")
        for k in self.kernels:
            self.w("/boot/vmlinuz-" + k)
            self.w("/boot/initrd.img-" + k)
            self.w("/lib/modules/%s/modules.alias" % k, "alias pci:v000010ECd00008168sv*sd*bc*sc*i* r8169\n" + (
                "alias pci:v000010ECd00008127sv*sd*bc*sc*i* r8127\n" if k == self.running or True else ""))
            self.w("/lib/modules/%s/build/Makefile" % k)
        # the new kernel's r8169 knows the RTL8127 (the situation the survey exists to show)
        self.w("/lib/modules/%s/modules.alias" % self.kernels[0],
               "alias pci:v000010ECd00008168sv*sd*bc*sc*i* r8169\nalias pci:v000010ECd00008127sv*sd*bc*sc*i* r8169\nalias pci:v000010ECd00008127sv*sd*bc*sc*i* r8127\n")
        os.makedirs(self.root + "/boot/grub", exist_ok=True)
        shutil.copy(self.grub_cfg_fixture, self.root + "/boot/grub/grub.cfg")
        self.w("/etc/default/grub", 'GRUB_DEFAULT=0\nGRUB_TIMEOUT_STYLE=hidden\nGRUB_TIMEOUT=0\nGRUB_CMDLINE_LINUX_DEFAULT="quiet splash"\n')
        os.makedirs(self.root + "/sys/firmware/efi", exist_ok=True)
        # two r8127 ports
        for i, (iface, slot) in enumerate((("enp1s0", "0000:01:00.0"), ("enp49s0", "0000:31:00.0"))):
            dev = "%s/sys/devices/pci0000:00/%s" % (self.root, slot)
            os.makedirs(dev, exist_ok=True)
            _wf(dev + "/vendor", "0x10ec\n")
            _wf(dev + "/device", "0x8127\n")
            drv = "%s/sys/bus/pci/drivers/r8127" % self.root
            os.makedirs(drv, exist_ok=True)
            os.symlink(drv, dev + "/driver")
            nd = "%s/sys/class/net/%s" % (self.root, iface)
            os.makedirs(nd, exist_ok=True)
            os.symlink(dev, nd + "/device")
            _wf(nd + "/speed", "10000\n")
            _wf(nd + "/operstate", "up\n")
        self.w("/sys/module/r8127/version", "11.015.00\n")
        self.w("/usr/src/r8127-11.015.00/dkms.conf", "PACKAGE_NAME=r8127\n")
        self.w("/lib/firmware/rtl_nic/rtl8125b-2.fw")
        self.w("/lib/firmware/rtl_nic/rtl8127a-1.fw")
        self.w("/etc/apt/sources.list.d/ubuntu.sources", "Types: deb\nURIs: http://ports.ubuntu.com/ubuntu-ports\nSuites: resolute\n")
        self.w("/etc/apt/sources.list.d/tailscale.list", "deb [signed-by=/usr/share/keyrings/tailscale.gpg] https://pkgs.tailscale.com/stable/ubuntu resolute main\n")
        self.w("/var/lib/apt/lists/ports.ubuntu.com_InRelease")
        self.w("/var/lib/dpkg/lock-frontend")
        self.w("/var/lib/dpkg/lock")
        self.w("/var/cache/apt/archives/lock")
        self.w("/var/lib/apt/lists/lock")
        # what an idle Ubuntu host always runs: neither is a package run (their comm is "unattended-upgr" and "packagekitd")
        self.proc(900, "/usr/bin/python3", "/usr/share/unattended-upgrades/unattended-upgrade-shutdown", "--wait-for-signal")
        self.proc(901, "/usr/libexec/packagekitd")
        self.w("/proc/1/comm", "systemd\n")
        self.w("/proc/stat", "btime 1\n")
        self.w("/etc/modprobe.d/blacklist.conf", "# nothing\n")
        self.w("/etc/needrestart/needrestart.conf", "# Restart mode: (l)ist only, (i)nteractive or (a)utomatically.\n#$nrconf{restart} = 'i';\n")
        os.makedirs(self.root + "/etc/needrestart/conf.d", exist_ok=True)
        self.build_snapshot_tree()
        self.build_boot_proof()
        self.w("/etc/netplan/50-cloud-init.yaml", "network:\n  version: 2\n")
        pk = {"linux-image-" + k: {"version": k, "status": "installed"} for k in self.kernels}
        pk.update({"linux-modules-" + self.running: {"version": self.running, "status": "installed"},
                   "linux-headers-" + self.running: {"version": self.running, "status": "installed"},
                   "linux-firmware": {"version": "20260301.git1", "status": "installed"},
                   "unattended-upgrades": {"version": "2.12", "status": "installed"},
                   "needrestart": {"version": "3.8", "status": "installed"},
                   "build-essential": {"version": "12.12", "status": "installed"},
                   "tailscale": {"version": "1.90.0", "status": "installed"},
                   "grub-efi-arm64-bin": {"version": "2.14", "status": "installed"}, "grub-common": {"version": "2.14", "status": "installed"},
                   "shim-signed": {"version": "1.60", "status": "installed"}, "openssh-server": {"version": "10.0", "status": "installed"}})
        modinfo = {}
        for k in self.kernels:
            modinfo[k + "/r8169"] = {"filename": "/lib/modules/%s/kernel/drivers/net/ethernet/realtek/r8169.ko.zst" % k}
        modinfo[self.running + "/r8127"] = {"filename": "/lib/modules/%s/updates/dkms/r8127.ko.zst" % self.running, "version": "11.015.00", "signer": ""}
        modinfo[self.kernels[0] + "/r8127"] = {"filename": "/lib/modules/%s/updates/dkms/r8127.ko.zst" % self.kernels[0], "version": "11.015.00", "signer": ""}
        self.state = {
            "packages": pk,
            "modinfo": modinfo,
            "dkms_status": ["r8127/11.015.00, %s, aarch64: installed" % self.running, "r8127/11.015.00, %s, aarch64: installed" % self.kernels[0]],
            "grubenv": {"saved_entry": "", "next_entry": ""},
            "boot_fs": "ext4", "boot_src": "/dev/nvme0n1p2", "boot_uuid": self.BOOT_UUID,
            "df": {"/": {"total": 500000000, "free": 300000000}, "/boot": {"total": 1800000, "free": 1200000},
                   "/boot/efi": {"total": 500000, "free": 480000}, "/var": {"total": 500000000, "free": 300000000}},
            "ntp": "yes",
            "services": {"is-active unattended-upgrades.service": "active", "is-enabled unattended-upgrades.service": "enabled",
                         "is-active tailscaled": "active", "is-enabled tailscaled": "enabled", "is-enabled ssh": "enabled", "is-active apcupsd": "active",
                         "is-enabled celeris-resume-check.service": "enabled",
                         "is-active apt-daily.timer": "active", "is-enabled apt-daily.timer": "enabled",
                         "is-active apt-daily-upgrade.timer": "active", "is-enabled apt-daily-upgrade.timer": "enabled"},
            "sim": ["Inst linux-image-7.0.0-40-generic (7.0.0-40.40 Ubuntu:26.04/resolute-updates [arm64])",
                    "Inst libc6 [2.43-0ubuntu1] (2.43-0ubuntu2 Ubuntu:26.04/resolute-updates [arm64])",
                    "Conf libc6 (2.43-0ubuntu2 Ubuntu:26.04/resolute-updates [arm64])",
                    "The following packages have been kept back:", "  linux-generic", "0 upgraded"],
            "sim_phased": [],
            "apt_config": {"APT::Periodic::Unattended-Upgrade": "1", "Unattended-Upgrade::Remove-Unused-Kernel-Packages": "true"},
            "upgrade_kernels": ["7.0.0-40-generic"],
            "upgrade_adds": {"libc6": "2.43-0ubuntu2"},
            "next_grub_cfg": None,
        }
        self.state["sim_phased"] = self.state["sim"] + ["Inst phased-pkg (2 Ubuntu:26.04/resolute-updates [arm64])"]
        self.save()

    BOOT_UUID = "aaaaaaaa-1111-2222-3333-444444444444"
    EFI_GLOBAL = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

    def efi_load_option(self, description, path):
        """The bytes of an EFI variable holding a load option: attributes, then EFI_LOAD_OPTION with one file-path node."""
        node_path = (path + "\0").encode("utf-16le")
        nodes = bytes([4, 4]) + (4 + len(node_path)).to_bytes(2, "little") + node_path + bytes([0x7F, 0xFF, 4, 0])
        opt = (1).to_bytes(4, "little") + len(nodes).to_bytes(2, "little") + (description + "\0").encode("utf-16le") + nodes
        return (7).to_bytes(4, "little") + opt

    def set_efi_boot(self, path="\\EFI\\ubuntu\\shimaa64.efi", current=1):
        d = self.root + "/sys/firmware/efi/efivars"
        os.makedirs(d, exist_ok=True)
        with open("%s/BootCurrent-%s" % (d, self.EFI_GLOBAL), "wb") as f:
            f.write((7).to_bytes(4, "little") + current.to_bytes(2, "little"))
        with open("%s/Boot%04X-%s" % (d, current, self.EFI_GLOBAL), "wb") as f:
            f.write(self.efi_load_option("ubuntu", path))

    def build_boot_proof(self):
        """What the survey needs to PROVE that GRUB booted this host and reads the grubenv Linux writes."""
        self.w("/proc/cmdline", "BOOT_IMAGE=/boot/vmlinuz-%s root=UUID=%s ro quiet splash\n" % (self.running, self.BOOT_UUID))
        self.set_efi_boot()
        self.w("/boot/efi/EFI/ubuntu/grub.cfg", "search.fs_uuid %s root hd0,gpt2\nset prefix=($root)'/boot/grub'\nconfigfile $prefix/grub.cfg\n" % self.BOOT_UUID)
        with open(self.root + "/boot/grub/grubenv", "w") as f:
            f.write(("# GRUB Environment Block\n").ljust(1024, "#"))

    def build_snapshot_tree(self):
        """What reboot_snapshot.sh reads: interrupts, the bond, a boot id, the power guard's files."""
        self.w("/proc/interrupts", " 135:  4050000000  0  ITS-MSI 4  Edge      enp49s0-8\n 167:  0  9  ITS-MSI 2  Edge      enp1s0-8\n")
        self.w("/sys/class/net/enp1s0/device/msi_irqs/167")
        self.w("/sys/class/net/enp49s0/device/msi_irqs/135")
        self.w("/proc/irq/167/smp_affinity_list", "1\n")
        self.w("/proc/irq/167/effective_affinity_list", "1\n")
        self.w("/proc/irq/135/smp_affinity_list", "0-11\n")
        self.w("/proc/irq/135/effective_affinity_list", "8\n")
        self.w("/proc/sys/kernel/random/boot_id", "aaaaaaaa-0000-0000-0000-000000000001\n")
        self.w("/proc/uptime", "100.0 100.0\n")
        self.w("/proc/net/bonding/bond0", "Bonding Mode: IEEE 802.3ad\n\nSlave Interface: enp1s0\nMII Status: up\n\nSlave Interface: enp49s0\nMII Status: up\n")
        self.w("/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown", "#!/bin/sh\n", 0o755)
        self.w("/usr/local/sbin/cluster-power-event", "#!/bin/sh\n", 0o755)
        self.w("/etc/tmpfiles.d/celeris-power.conf", "d /run/celeris 0755 mini mini -\n")
        os.makedirs(self.root + "/run/celeris", exist_ok=True)
        os.makedirs(self.root + "/var/crash", exist_ok=True)

    def save(self):
        with open(os.path.join(self.state_dir, "state.json"), "w") as f:
            json.dump(self.state, f)

    def load(self):
        with open(os.path.join(self.state_dir, "state.json")) as f:
            self.state = json.load(f)
        return self.state

    # ---- running
    def env(self, extra=None):
        e = dict(os.environ)
        e.update({"PATH": self.bin + os.pathsep + e["PATH"], "OSU_ROOT": self.root, "STUB_STATE": self.state_dir, "STUB_LOG": self.log})
        if extra:
            e.update(extra)
        return e

    def run(self, *args, extra_env=None):
        r = subprocess.run([sys.executable, SCRIPT] + list(args), capture_output=True, text=True, env=self.env(extra_env), timeout=120)
        return r

    def calls(self):
        with open(self.log) as f:
            return [l.rstrip("\n") for l in f]

    def snapshot(self):
        """Every file under the fake root with its content: to prove a read-only run wrote nothing."""
        out = {}
        for d, _, files in os.walk(self.root):
            for f in files:
                p = os.path.join(d, f)
                try:
                    out[p] = (os.path.getsize(p), open(p, "rb").read() if os.path.isfile(p) and not os.path.islink(p) else b"")
                except OSError:
                    out[p] = None
        for d, dirs, _ in os.walk(self.root):
            for x in dirs:
                out[os.path.join(d, x) + "/"] = None
        return out

    def set_saved(self, kernel):
        sys.path.insert(0, os.path.join(HERE, "..", "files"))
        import importlib
        os.environ["OSU_ROOT"] = self.root
        m = importlib.import_module("os_upgrade")
        m.ROOT = self.root
        eid = m.entry_id_for(m.parse_grub_cfg(open(self.grub_cfg_fixture).read()), kernel)
        self.load()
        self.state["grubenv"]["saved_entry"] = eid
        self.save()
        return eid
