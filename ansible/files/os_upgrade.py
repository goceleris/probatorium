#!/usr/bin/env python3
"""os_upgrade.py - every command ansible/os-upgrade.yml runs ON a cluster host, in one auditable file.

Run by ansible.builtin.script as root (python 3 standard library only). The playbook has no shell or command
task: a host is read, changed and rebooted only through the named subcommands below, so this table is the whole
list of what the sanctioned OS upgrade can do to a machine (README.md, "Upgrading a cluster host's OS").

READ-ONLY subcommands (the survey mode of the workflow uses only these; READ_ONLY below makes the process
refuse to start any command that is not on the read-only allow-list, and the tests run it against a fake root and
check that nothing was written):
    survey            one JSON object: os-release, kernels, grub, packages, dkms, NICs, r8127 vs in-tree, disk, time
    simulate          the JSON of `apt-get -s full-upgrade` (installs, removals, kept back) on the lists as they are
    dkms-check        (without --build) is the NEW kernel ready: modules, initrd, grub entry, signature, fallback
    dump WHAT         packages | policy | sysctl | cmdline | grub | dpkg-l | held | kernel : plain text, for the evidence
    upgrade-status    state of the detached apt run (running | done | died | none) and its exit code; --wait N waits on the host
    upgrade-log       the log of the detached apt run
    marker            the record of the last verified OS upgrade of this host ({} when none)

HOST-CHANGING subcommands (the upgrade mode only, in this order; each one is the only thing its task does):
    apt-update                  apt-get update (writes /var/lib/apt/lists and /var/cache/apt/*.bin, runs the APT::Update::Post-Invoke hooks;
                                the survey mode may also use it, when asked)
    pin-grub                    GRUB_DEFAULT=saved in a drop-in, update-grub, grub-set-default <RUNNING kernel>, and the
                                running kernel's packages marked manual: BEFORE a new kernel is installed, so that a
                                power-cycle at any later point boots the old kernel
    upgrade-start               detached `apt-get full-upgrade` (non-interactive, --force-confold, no service restarts
                                behind a run, no needrestart restart); never autoremove
    blacklist-r8169             (only when asked) /etc/modprobe.d/celeris-r8127.conf: the in-tree r8169 must not race the
                                out-of-tree r8127 for the same PCI id on the new kernel
    dkms-check --build          the check above, building the DKMS modules for the new kernel first when they are missing
    arm-oneshot                 grub-reboot <new kernel>: the next boot only; saved default stays the old kernel
    disarm                      clears a pending one-shot (grub-editenv - unset next_entry): after any failure between
                                arm-oneshot and the new kernel's boot, and as a mode of its own
    make-permanent              grub-set-default <new kernel> and the marker: ONLY after the verification passed, and only on the
                                host that RUNS that kernel
    repair-start                (mode=repair) detached `dpkg --configure -a`, after purging the half-installed kernels newer than the
                                running one when --action abort-new-kernel, or --action release-holds: releases the access-path holds
                                a dead run of this tool left (recorded in held.txt); upgrade-status/upgrade-log read it
    upgrade-cleanup             removes OUR policy-rc.d once the detached run is over (never while its process is alive)
    freeze / thaw               (mode=freeze, mode=thaw) the platform stays FIXED for the evidence campaign: freeze records what it will change, stops and
                                disables apt-daily.timer, apt-daily-upgrade.timer and unattended-upgrades.service and sets needrestart to list-only
                                (no automatic service restart), and records the platform (kernel, a hash of every installed package and version, the holds);
                                thaw puts back exactly what freeze recorded it changed, and nothing else
    hold-access / release-access  apt-mark hold of the access-path packages BEFORE the simulation (so the plan judged is the plan that
                                runs), recorded in the run's held.txt; released by the run, or by repair release-holds when it died

Environment: OSU_ROOT is a test override, a prefix for every path read or written. Empty on a host.
Exit 0 whatever a read found (a finding is data); 1 when a host-changing step could not do what it says.
"""
import argparse
import fcntl
import glob
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = os.environ.get("OSU_ROOT", "")
STATE_DIR = "/var/lib/celeris-os-upgrade"
LOG_DIR = "/var/log/celeris-os-upgrade"
DROPIN = "/etc/default/grub.d/99-celeris-os-upgrade.cfg"
POLICY_RC_D = "/usr/sbin/policy-rc.d"
MODPROBE_CONF = "/etc/modprobe.d/celeris-r8127.conf"
READ_ONLY = False
SBIN = "/usr/local/sbin:/usr/sbin:/sbin"

PCI_REALTEK = "10ec"


def p(path):
    return ROOT + path


def rd(path, default=""):
    try:
        with open(p(path), "r", errors="replace") as f:
            return f.read().strip()
    except OSError:
        return default


# ---------------------------------------------------------------------------------------------- commands
def ro_allowed(a):
    """True when argv is a command that cannot change the host."""
    n = os.path.basename(a[0])
    rest = a[1:]
    if n in ("dpkg-query", "modinfo", "findmnt", "df", "apt-config", "apt-cache", "uname"):
        return True
    if n == "dpkg":
        return bool(rest) and rest[0] in ("--audit", "-S", "-L", "-s", "-l")
    if n == "apt-get":
        return any(x in ("-s", "--simulate", "--just-print", "--dry-run") for x in rest)
    if n == "apt-mark":
        return rest[:1] == ["showhold"]
    if n == "dkms":
        return rest[:1] == ["status"]
    if n == "grub-editenv":
        return rest[-1:] == ["list"]
    if n == "systemctl":
        return rest[:1] in (["is-active"], ["is-enabled"], ["is-system-running"], ["show"])
    if n == "timedatectl":
        return rest[:1] == ["show"]
    if n == "mokutil":
        return rest == ["--sb-state"]
    if n == "sysctl":
        return rest == ["-a"]
    if n == "sshd":
        return rest == ["-t"]
    if n == "tailscaled":
        return rest == ["--version"]
    if n == "netplan":
        return rest[:1] == ["generate"] and "--root-dir" in rest
    if n in ("gcc", "g++", "clang", "cmake", "make", "python3", "docker", "tailscale", "cc"):
        return rest == ["--version"]
    return False


def run(argv, timeout=600, env=None, check_ro=True):
    """(rc, stdout, stderr). A missing command is rc 127, never an exception."""
    if READ_ONLY and check_ro and not ro_allowed(argv):
        raise SystemExit("read-only violation: refusing to run " + " ".join(shlex.quote(x) for x in argv))
    e = dict(os.environ)
    e["LC_ALL"] = "C"
    e["PATH"] = e.get("PATH", "") + os.pathsep + SBIN
    if env:
        e.update(env)
    try:
        r = subprocess.run(argv, capture_output=True, text=True, errors="replace", timeout=timeout, env=e)
        return r.returncode, r.stdout, r.stderr
    except FileNotFoundError:
        return 127, "", argv[0] + ": not found"
    except subprocess.TimeoutExpired as x:
        return 124, (x.stdout or "") if isinstance(x.stdout, str) else "", "timeout after %ss" % timeout


def have(cmd):
    return shutil.which(cmd, path=os.environ.get("PATH", "") + os.pathsep + SBIN) is not None


def write(path, content, mode=0o644):
    if READ_ONLY:
        raise SystemExit("read-only violation: refusing to write " + path)
    full = p(path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    tmp = full + ".tmp"
    with open(tmp, "w") as f:
        f.write(content)
    os.chmod(tmp, mode)
    os.replace(tmp, full)


def emit(obj):
    sys.stdout.write(json.dumps(obj, indent=1, sort_keys=True) + "\n")


# ---------------------------------------------------------------------------------------------- kernels
KVER = re.compile(r"^(\d+)\.(\d+)\.(\d+)-(\d+)-(\S+)$")


def kver_key(v):
    m = KVER.match(v)
    return tuple(int(x) for x in m.groups()[:4]) if m else (0, 0, 0, 0)


def flavor(v):
    m = KVER.match(v)
    return m.group(5) if m else ""


def running_kernel():
    return rd("/proc/sys/kernel/osrelease") if ROOT else os.uname().release


def installed_kernels():
    rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package} ${Status}\n", "linux-image-*"])
    found = set()
    for line in out.splitlines():
        parts = line.split()
        if len(parts) < 4 or parts[-1] != "installed":
            continue
        m = re.match(r"^linux-image-(?:unsigned-)?(\d+\.\d+\.\d+-\d+-\S+)$", parts[0])
        if m:
            found.add(m.group(1))
    return sorted(found, key=kver_key)


def vmlinuz_versions():
    return sorted((os.path.basename(x)[len("vmlinuz-"):] for x in glob.glob(p("/boot/vmlinuz-*"))), key=kver_key)


# ---------------------------------------------------------------------------------------------- grub
MENU = re.compile(r"""^\s*(menuentry|submenu)\s+(?:'([^']*)'|"([^"]*)")(.*?)\{\s*$""")
ENTRY_ID = re.compile(r"""(?:\$menuentry_id_option|--id)\s+'([^']+)'""")
LINUX = re.compile(r"^\s*linux(?:efi|16)?\s+\S*/vmlinuz-(\S+)")


def parse_grub_cfg(text):
    """Menu entries of a generated grub.cfg: [{title, id, full_id, parent, kernel, recovery}]."""
    entries, stack = [], []
    for line in text.splitlines():
        m = MENU.match(line)
        if m:
            kind, title, rest = m.group(1), m.group(2) if m.group(2) is not None else m.group(3), m.group(4)
            i = ENTRY_ID.search(rest)
            node = {"kind": kind, "title": title, "id": i.group(1) if i else "", "kernel": "", "parent": ""}
            for s in reversed(stack):
                if s["kind"] == "submenu":
                    node["parent"] = s["id"]
                    break
            stack.append(node)
            continue
        if line.strip() == "}":
            if stack:
                node = stack.pop()
                if node["kind"] == "menuentry":
                    node["recovery"] = "recovery" in node["id"] or "recovery mode" in node["title"]
                    node["full_id"] = (node["parent"] + ">" + node["id"]) if node["parent"] else node["id"]
                    entries.append(node)
            continue
        if line.rstrip().endswith("{"):
            stack.append({"kind": "other", "id": "", "title": "", "kernel": "", "parent": ""})
            continue
        if stack and stack[-1]["kind"] == "menuentry" and not stack[-1]["kernel"]:
            lm = LINUX.match(line)
            if lm:
                stack[-1]["kernel"] = lm.group(1)
    return entries


def grub_entries():
    return parse_grub_cfg(rd("/boot/grub/grub.cfg"))


def entry_id_for(entries, kernel):
    """The id to give grub-set-default / grub-reboot for KERNEL: its non-recovery entry in the Advanced submenu,
    else the top-level one. '' when the menu has none."""
    cand = [e for e in entries if e["kernel"] == kernel and not e["recovery"]]
    for e in cand:
        if e["parent"]:
            return e["full_id"]
    return cand[0]["full_id"] if cand else ""


def kernel_of_entry(entries, ref):
    """The kernel a grub entry reference (a full id, an id or a title) names, '' when unknown."""
    for e in entries:
        if ref in (e["full_id"], e["id"], e["title"]) and e["kernel"]:
            return e["kernel"]
    return ""


def grub_settings():
    """Effective GRUB_* assignments: /etc/default/grub, then /etc/default/grub.d/*.cfg in order (later wins)."""
    files = [p("/etc/default/grub")] + sorted(glob.glob(p("/etc/default/grub.d/*.cfg")))
    out, used = {}, []
    for f in files:
        try:
            text = open(f, errors="replace").read()
        except OSError:
            continue
        used.append(f[len(ROOT):] if ROOT else f)
        for line in text.splitlines():
            m = re.match(r"^\s*(GRUB_[A-Z_0-9]+)=(.*)$", line)
            if m:
                out[m.group(1)] = m.group(2).strip().strip("'\"")
    return out, used


def grub_editenv():
    rc, out, _ = run(["grub-editenv", "list"])
    env = {}
    if rc == 0:
        for line in out.splitlines():
            k, _, v = line.partition("=")
            if k:
                env[k] = v
    return env


def cfg_recordfail_timeout(text):
    m = re.search(r'recordfail\}?"\s*=\s*1\s*\]\s*;\s*then\s*\n\s*set timeout=(-?\d+)', text)
    return m.group(1) if m else ""


def efi_boot_current():
    """The loader the firmware booted (BootCurrent -> its Boot#### load option), read from the EFI variables. None
    when the variables are not there (not an EFI boot, or unreadable)."""
    d = p("/sys/firmware/efi/efivars")
    bc = sorted(glob.glob(d + "/BootCurrent-*"))
    if not bc:
        return None
    try:
        with open(bc[0], "rb") as f:
            num = int.from_bytes(f.read()[4:6], "little")
        ents = sorted(glob.glob("%s/Boot%04X-*" % (d, num)))
        if not ents:
            return {"number": num, "paths": []}
        with open(ents[0], "rb") as f:
            data = f.read()[4:]  # skip the variable attributes; the load option starts here
    except OSError:
        return None
    # EFI_LOAD_OPTION: attributes u32, file path list length u16, description UTF-16 NUL, then device-path nodes whose
    # file-path nodes hold UTF-16 text. Every field up to the nodes is 2-byte aligned, so decode from the start.
    text = data.decode("utf-16le", errors="ignore")
    return {"number": num, "paths": re.findall(r"\\EFI\\[^\x00-\x1f]*?\.efi", text, re.I)}


GRUB_LOADER = re.compile(r"^\\EFI\\[^\\]+\\(shim|grub)[^\\]*\.efi$", re.I)


def esp_stub_prefix(loader):
    """(uuid, prefix) from the grub.cfg stub on the ESP beside the loader GRUB was started from."""
    m = re.match(r"^\\EFI\\([^\\]+)\\", loader)
    if not m:
        return "", ""
    text = rd("/boot/efi/EFI/%s/grub.cfg" % m.group(1))
    u = re.search(r"search\.fs_uuid\s+(\S+)", text)
    pr = re.search(r"set prefix=\(\$root\)'?([^'\s]+)'?", text)
    return (u.group(1).lower() if u else ""), (pr.group(1) if pr else "")


def boot_proof(running, efi, boot_uuid):
    """Did GRUB boot this host, and does GRUB read the grubenv that grub-editenv writes? Everything one-shot and every
    fallback depends on both; the presence of the grub tools proves neither. Unknown counts as not proven."""
    problems = []
    cmd = rd("/proc/cmdline")
    m = re.search(r"(?:^|\s)BOOT_IMAGE=(\S+)", cmd)
    image = m.group(1) if m else ""
    if not image:
        problems.append("/proc/cmdline has no BOOT_IMAGE=: GRUB's linux command adds it, U-Boot and systemd-boot do not (cmdline: %s)" % (cmd[:120] or "unreadable"))
    elif not re.search(r"vmlinuz-%s$" % re.escape(running), image):
        problems.append("BOOT_IMAGE=%s does not name vmlinuz-%s" % (image, running))
    loader, prefix, stub_uuid = "", "", ""
    if efi:
        cur = efi_boot_current()
        paths = (cur or {}).get("paths", [])
        if cur is None:
            problems.append("the EFI variables are not readable: cannot tell which loader the firmware booted")
        else:
            gl = [x for x in paths if GRUB_LOADER.match(x)]
            if not gl:
                problems.append("the firmware booted %s (Boot%04X), not shim or grub under \\EFI\\<vendor>" % (", ".join(paths) or "an unknown loader", cur["number"]))
            else:
                loader = gl[0]
                stub_uuid, prefix = esp_stub_prefix(loader)
                if not stub_uuid or not prefix:
                    problems.append("the grub.cfg stub on the ESP beside %s has no readable search/prefix: cannot tell which filesystem GRUB reads grubenv from" % loader)
                elif prefix.rstrip("/") not in ("/boot/grub", "/grub"):
                    problems.append("GRUB's prefix is %s, not /boot/grub: it reads another grubenv than grub-editenv writes" % prefix)
                elif not boot_uuid:
                    problems.append("the filesystem of /boot/grub has no readable UUID: cannot compare it with GRUB's search")
                elif stub_uuid != boot_uuid.lower():
                    problems.append("GRUB reads its environment from filesystem %s, but /boot/grub is on %s: grub-reboot and grub-set-default would write a file GRUB never reads" % (stub_uuid, boot_uuid))
    genv = p("/boot/grub/grubenv")
    try:
        st = os.lstat(genv)
        if not os.path.isfile(genv) or os.path.islink(genv):
            problems.append("/boot/grub/grubenv is not a regular file")
        elif st.st_size != 1024:
            problems.append("/boot/grub/grubenv is %d bytes, not 1024" % st.st_size)
    except OSError:
        problems.append("/boot/grub/grubenv does not exist")
    return {"ok": not problems, "problems": problems, "boot_image": image, "efi_loader": loader, "prefix": prefix, "stub_uuid": stub_uuid,
            "boot_uuid": boot_uuid.lower()}


def boot_info():
    cfg = rd("/boot/grub/grub.cfg")
    settings, files = grub_settings()
    entries = parse_grub_cfg(cfg)
    env = grub_editenv()
    saved = env.get("saved_entry", "")
    uses_saved = 'set default="${saved_entry}"' in cfg
    if uses_saved:
        default_kernel = kernel_of_entry(entries, saved) if saved else ""
    else:
        top = [e for e in entries if not e["parent"] and e["kernel"]]
        default_kernel = top[0]["kernel"] if top and settings.get("GRUB_DEFAULT", "0") == "0" else ""
    # what a GRUB that does not read the environment block boots: the FIRST menu entry (default="0", or the fallback when the
    # saved entry is unusable). With the newest kernel installed it is that kernel, not the running one.
    top0 = [e for e in entries if not e["parent"] and e["kernel"]]
    entry0_kernel = top0[0]["kernel"] if top0 else ""
    rc, fstype, _ = run(["findmnt", "-n", "-o", "FSTYPE", "-T", p("/boot/grub")])
    rc2, src, _ = run(["findmnt", "-n", "-o", "SOURCE", "-T", p("/boot/grub")])
    src = src.strip()
    rc4, uuid_out, _ = run(["findmnt", "-n", "-o", "UUID", "-T", p("/boot/grub")])
    rc4_uuid = uuid_out.strip()
    stack = ""
    if src.startswith("/dev/mapper/") or src.startswith("/dev/md") or src.startswith("/dev/dm-"):
        stack = "lvm-or-mdraid"
    if fstype.strip() in ("btrfs", "zfs"):
        stack = fstype.strip()
    efi = os.path.isdir(p("/sys/firmware/efi"))
    sb = "not-efi"
    if efi:
        sb = "unknown"
        for f in glob.glob(p("/sys/firmware/efi/efivars/SecureBoot-*")):
            try:
                b = open(f, "rb").read()
                sb = "enabled" if b and b[-1] == 1 else "disabled"
            except OSError:
                pass
        if sb == "unknown" and have("mokutil"):
            rc3, o, _ = run(["mokutil", "--sb-state"])
            sb = "enabled" if "enabled" in o.lower() else ("disabled" if "disabled" in o.lower() else "unknown")
    return {
        "efi": efi,
        "secure_boot": sb,
        "grub_cfg_present": bool(cfg),
        "grub_reboot": have("grub-reboot"),
        "grub_set_default": have("grub-set-default"),
        "grub_editenv": have("grub-editenv"),
        "update_grub": have("update-grub"),
        "settings": {k: v for k, v in settings.items() if k in (
            "GRUB_DEFAULT", "GRUB_SAVEDEFAULT", "GRUB_TIMEOUT", "GRUB_TIMEOUT_STYLE", "GRUB_RECORDFAIL_TIMEOUT",
            "GRUB_DISABLE_RECOVERY", "GRUB_CMDLINE_LINUX", "GRUB_CMDLINE_LINUX_DEFAULT")},
        "settings_files": files,
        "savedefault": settings.get("GRUB_SAVEDEFAULT", "") == "true",
        "cfg_uses_saved": uses_saved,
        "cfg_recordfail_timeout": cfg_recordfail_timeout(cfg),
        "saved_entry": saved,
        "next_entry": env.get("next_entry", ""),
        "dropin_present": os.path.exists(p(DROPIN)),
        "default_kernel": default_kernel,
        "entry0_kernel": entry0_kernel,
        "entries": [{"kernel": e["kernel"], "id": e["full_id"], "title": e["title"]} for e in entries if e["kernel"] and not e["recovery"]],
        "boot_fs": fstype.strip(),
        "boot_source": src,
        "boot_stack": stack,
        "proof": boot_proof(running_kernel(), efi, rc4_uuid),
    }


# ---------------------------------------------------------------------------------------------- dkms
DKMS = re.compile(r"^([^/,\s]+)[/,]\s*([^,\s:]+),\s*([^,\s:]+)(?:,\s*([^:\s]+))?:\s*(\S+)")


def dkms_modules():
    rc, out, _ = run(["dkms", "status"])
    mods = []
    if rc != 0:
        return mods
    for line in out.splitlines():
        m = DKMS.match(line.strip())
        if m:
            mods.append({"name": m.group(1), "version": m.group(2), "kernel": m.group(3), "arch": m.group(4) or "", "state": m.group(5)})
    return mods


def dkms_installed_for(kernel):
    return sorted({m["name"] for m in dkms_modules() if m["kernel"] == kernel and m["state"] in ("installed", "built")})


def modinfo(kernel, module):
    """What modinfo -k KERNEL says of MODULE: found, version, filename, signer."""
    rc, fn, _ = run(["modinfo", "-k", kernel, "-F", "filename", module])
    fn = fn.strip()
    info = {"found": rc == 0 and bool(fn), "filename": fn, "version": "", "signer": ""}
    if info["found"]:
        info["version"] = run(["modinfo", "-k", kernel, "-F", "version", module])[1].strip()
        info["signer"] = run(["modinfo", "-k", kernel, "-F", "signer", module])[1].strip()
    return info


def builtin_module(kernel, module):
    b = rd("/lib/modules/%s/modules.builtin" % kernel)
    return bool(re.search(r"/%s\.ko(?:\.\w+)?$" % re.escape(module), b, re.M))


def pci_claims(kernel, pci_id):
    """Modules whose aliases in KERNEL's modules.alias match the PCI id vendor:device."""
    v, d = pci_id.split(":")
    pat = re.compile(r"^alias pci:v0000%sd0000%s\S*\s+(\S+)\s*$" % (v.upper(), d.upper()), re.M)
    return sorted(set(pat.findall(rd("/lib/modules/%s/modules.alias" % kernel))))


# ---------------------------------------------------------------------------------------------- NICs
def nics():
    out = []
    for d in sorted(glob.glob(p("/sys/class/net/*"))):
        dev = os.path.join(d, "device")
        if not os.path.exists(dev):
            continue
        iface = os.path.basename(d)
        drv = ""
        if os.path.islink(os.path.join(dev, "driver")):
            drv = os.path.basename(os.readlink(os.path.join(dev, "driver")))
        vendor = rd(dev[len(ROOT):] + "/vendor").replace("0x", "")
        device = rd(dev[len(ROOT):] + "/device").replace("0x", "")
        speed = rd(d[len(ROOT):] + "/speed")
        out.append({
            "iface": iface,
            "driver": drv,
            "driver_version": rd("/sys/module/%s/version" % drv) if drv else "",
            "pci_id": ("%s:%s" % (vendor, device)) if vendor and device else "",
            "pci_slot": os.path.basename(os.path.realpath(dev)),
            "speed_mbps": int(speed) if re.match(r"^-?\d+$", speed) else None,
            "operstate": rd(d[len(ROOT):] + "/operstate"),
        })
    return out


def r8127_report(kernels, running, nic_list):
    ids = sorted({n["pci_id"] for n in nic_list if n["driver"] == "r8127" and n["pci_id"]}) or [PCI_REALTEK + ":8127"]
    mods = [m for m in dkms_modules() if m["name"] == "r8127"]
    src_dirs = sorted(glob.glob(p("/usr/src/r8127-*")))
    pkg = ""
    for d in src_dirs:
        rc, out, _ = run(["dpkg", "-S", d[len(ROOT):] if ROOT else d])
        if rc == 0 and ":" in out:
            pkg = out.split(":")[0].strip()
            break
    if not pkg:
        rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package}\n", "r8127*"])
        pkg = out.split()[0] if rc == 0 and out.split() else ""
    cur = modinfo(running, "r8127") if running else {"found": False}
    if pkg:
        source = "package:" + pkg
    elif mods or src_dirs:
        source = "dkms-source-without-package"
    elif cur.get("found"):
        source = "manual-build"
    else:
        source = "none"
    per_kernel = {}
    for k in kernels:
        r = modinfo(k, "r8127")
        r["dkms_state"] = sorted({m["state"] for m in mods if m["kernel"] == k})
        per_kernel[k] = {
            "r8127": r,
            "r8169": {"found": modinfo(k, "r8169")["found"], "builtin": builtin_module(k, "r8169")},
            "claims": {i: pci_claims(k, i) for i in ids},
            "intree_lists_id": {i: "r8169" in pci_claims(k, i) for i in ids},
        }
    fw = sorted(os.path.basename(f) for f in glob.glob(p("/lib/firmware/rtl_nic/*8127*")))
    allfw = glob.glob(p("/lib/firmware/rtl_nic/*"))
    bl = []
    for f in sorted(glob.glob(p("/etc/modprobe.d/*.conf"))):
        for line in open(f, errors="replace").read().splitlines():
            if re.search(r"\b(r8169|r8127)\b", line) and not line.strip().startswith("#"):
                bl.append(os.path.basename(f) + ": " + line.strip())
    return {
        "present": any(n["driver"] == "r8127" for n in nic_list) or bool(mods) or bool(src_dirs),
        "pci_ids": ids,
        "loaded_version": rd("/sys/module/r8127/version"),
        "source": source,
        "dkms": mods,
        "dkms_source_dirs": [d[len(ROOT):] for d in src_dirs],
        "bound": {n["iface"]: n["driver"] for n in nic_list if n["pci_id"].startswith(PCI_REALTEK + ":")},
        "per_kernel": per_kernel,
        "r8169_blacklisted": any(re.search(r"^\s*(blacklist|install)\s+r8169\b", x.split(": ", 1)[1]) for x in bl),
        "modprobe_lines": bl,
        "firmware_rtl_nic_files": len(allfw),
        "firmware_rtl8127_files": fw,
        "linux_firmware": dpkg_version("linux-firmware"),
    }


# ---------------------------------------------------------------------------------------------- packages
def dpkg_version(pkg):
    rc, out, _ = run(["dpkg-query", "-W", "-f", "${Version}", pkg])
    return out.strip() if rc == 0 else ""


INST = re.compile(r"^Inst (\S+)(?: \[([^\]]+)\])? \((\S+) ")
REMV = re.compile(r"^Remv (\S+)")


# what apt must never remove on a cluster host; the SAME text as os_protected_regex in vars/os_upgrade_judge.yml (a test compares them)
PROTECTED_RE = re.compile(r"^(linux-|grub|shim|dkms|openssh|tailscale|apcupsd|docker|containerd|runc|netplan|systemd|ifenslave|r8127|network|sudo|python3|udev|kmod|initramfs-tools|iproute2|libc6|ca-certificates|efibootmgr|apt$|dpkg$|libapt|openssl|libssl|mokutil|ethtool|curl$)")


def simulate(include_phased=False):
    argv = ["apt-get", "-s", "-o", "Debug::NoLocking=true", "-o", "APT::Get::Assume-Yes=false"]
    if include_phased:
        argv += ["-o", "APT::Get::Always-Include-Phased-Updates=true"]
    rc, out, err = run(argv + ["full-upgrade"], env={"DEBIAN_FRONTEND": "noninteractive"})
    inst, remv, kept = [], [], []
    keeping = False
    for line in out.splitlines():
        m = INST.match(line)
        if m:
            inst.append({"name": m.group(1), "old": m.group(2) or "", "new": m.group(3)})
            continue
        m = REMV.match(line)
        if m:
            remv.append(m.group(1))
            continue
        if line.startswith("The following packages have been kept back"):
            keeping = True
            continue
        if keeping:
            if line.startswith(" "):
                kept += line.split()
            else:
                keeping = False
    return {"rc": rc, "inst": inst, "remv": remv, "kept_back": kept, "protected_removals": [r for r in remv if PROTECTED_RE.match(r)],
            "kernel_inst": [i["name"] for i in inst if re.match(r"^linux-(image|modules|headers)", i["name"])],
            "error": "" if rc == 0 else (err.strip() or out.strip())[-400:]}


def sources():
    repos = []
    files = [p("/etc/apt/sources.list")] + sorted(glob.glob(p("/etc/apt/sources.list.d/*")))
    for f in files:
        try:
            text = open(f, errors="replace").read()
        except OSError:
            continue
        for u in re.findall(r"^\s*deb(?:-src)?\s+(?:\[[^\]]*\]\s+)?(\S+)", text, re.M) + re.findall(r"^URIs:\s*(.+)$", text, re.M):
            for uri in u.split():
                uri = re.sub(r"^(\w+://)[^/@]*@", r"\1", uri)  # these records are published: never a credential
                host = re.sub(r"^\w+://", "", uri).split("/")[0]
                official = bool(re.search(r"(^|\.)(ubuntu\.com|debian\.org|canonical\.com)$", host))
                repos.append({"file": os.path.basename(f), "uri": uri, "official": official})
    return repos


def lists_age_hours():
    newest = 0
    for f in glob.glob(p("/var/lib/apt/lists/*")):
        try:
            if os.path.isfile(f):
                newest = max(newest, os.path.getmtime(f))
        except OSError:
            pass
    return round((time.time() - newest) / 3600, 1) if newest else None


# What counts as a package run: a process that EXECUTES one of these programs (or a script of that name). The name is read from
# /proc/<pid>/cmdline and NEVER decided from /proc/<pid>/comm: the kernel keeps 15 characters of a name, and "unattended-upgr" is
# exactly 15, so the idle daemon Ubuntu always runs (`unattended-upgrade-shutdown --wait-for-signal`, waiting for a shutdown signal) and
# a real `unattended-upgrade` run read alike. packagekitd idles when nobody uses it. Neither is a package run: when either one is
# working it holds the dpkg or apt locks, which are checked next. (Release 1.6.0's first survey judged both by comm and
# refused every host forever: probatorium run 39814880117.) The same words are in vars/os_upgrade_judge.yml (os_busy_process_names)
# and .github/scripts/os-upgrade-summary.sh (busy_names): a test compares them.
PKG_RUN_NAMES = frozenset("apt apt-get aptitude dpkg unattended-upgrade apt.systemd.daily dkms update-grub grub-mkconfig update-initramfs".split())
SCRIPT_RUNNERS = re.compile(r"^(python[0-9.]*|perl|sh|bash|dash)$")
APT_LOCKS = ("/var/lib/dpkg/lock-frontend", "/var/lib/dpkg/lock", "/var/lib/apt/lists/lock", "/var/cache/apt/archives/lock")


def process_program(argv):
    """The program a process runs: argv[0]'s base name, or, for a script started through its interpreter (python3
    /usr/bin/unattended-upgrade, /bin/sh /usr/lib/apt/apt.systemd.daily), the script's. "" for no argv (a kernel thread, a zombie)."""
    if not argv:
        return ""
    name = os.path.basename(argv[0])
    if SCRIPT_RUNNERS.match(name):
        for a in argv[1:]:
            if a == "-c":
                return name  # sh -c "...": a command string, not a script
            if a.startswith("-"):
                continue
            return os.path.basename(a)
    return name


def package_processes():
    """The package-manager programs running now, by name, sorted: real apt, apt-get, dpkg, unattended-upgrade ... processes, read from
    /proc/<pid>/cmdline. The idle unattended-upgrade-shutdown listener and an idle packagekitd are not among them."""
    found = []
    for c in glob.glob(p("/proc/[0-9]*/cmdline")):
        try:
            with open(c, "rb") as f:
                raw = f.read()
        except OSError:
            continue
        name = process_program([x.decode("utf-8", "replace") for x in raw.split(b"\0") if x])
        if name in PKG_RUN_NAMES:
            found.append(name)
    return sorted(found)


def locks_held():
    """The dpkg and apt lock files another process holds right now (apt, dpkg, unattended-upgrade and PackageKit all take them while they
    work, and not while they idle). A shared lock is asked for without waiting: a refusal means somebody holds it exclusively."""
    held = []
    for lock in APT_LOCKS:
        try:
            with open(p(lock), "rb") as f:
                try:
                    fcntl.lockf(f, fcntl.LOCK_SH | fcntl.LOCK_NB)
                except OSError:
                    held.append(lock)
        except OSError:
            continue
    return held


def package_run_busy():
    """"" when no package run is going on, else the words for a refusal: the programs running, or the locks held."""
    procs = package_processes()
    if procs:
        return ", ".join(procs)
    held = locks_held()
    return ("an apt or dpkg lock is held (%s)" % ", ".join(held)) if held else ""


def apt_config():
    rc, out, _ = run(["apt-config", "dump"])
    cfg = {}
    for line in out.splitlines():
        m = re.match(r'^(\S+)\s+"([^"]*)";', line)
        if m:
            cfg[m.group(1)] = m.group(2)
    return cfg


def svc(unit):
    a = run(["systemctl", "is-active", unit])[1].strip()
    e = run(["systemctl", "is-enabled", unit])[1].strip()
    return {"active": a, "enabled": e}


def df(path):
    rc, out, _ = run(["df", "-Pk", p(path)])
    lines = out.strip().splitlines()
    if rc != 0 or len(lines) < 2:
        return None
    f = lines[-1].split()
    try:
        return {"total_kib": int(f[1]), "free_kib": int(f[3]), "mount": f[-1]}
    except (IndexError, ValueError):
        return None


HOST_PKGS = ["linux-generic", "linux-image-generic", "linux-headers-generic", "linux-firmware", "build-essential", "gcc", "g++", "clang",
             "cmake", "make", "python3", "docker.io", "containerd", "runc", "tailscale", "apcupsd", "dkms", "needrestart",
             "unattended-upgrades", "grub-efi-arm64", "grub-efi-amd64", "grub-efi-arm64-signed", "grub-efi-amd64-signed", "shim-signed",
             "libc6", "openssh-server", "systemd", "curl", "ca-certificates", "git", "r8127-dkms"]


def host_toolchain():
    pk = {x: dpkg_version(x) for x in HOST_PKGS}
    bins = {}
    for b in ("gcc", "g++", "clang", "cmake", "make", "python3", "docker", "tailscale"):
        if have(b):
            bins[b] = (run([b, "--version"])[1].splitlines() or [""])[0].strip()
    return {"packages": {k: v for k, v in pk.items() if v}, "binaries": bins}


def kver_prefix(kernel):
    """"7.0.0-40" of "7.0.0-40-generic" or "7.0.0-40-generic-64k": what a kernel's packages share, whatever the flavor."""
    m = re.match(r"^(\d+\.\d+\.\d+-\d+)", kernel or "")
    return m.group(1) if m else ""


def armed_records():
    """Kernels a run armed as a one-shot and that no run made permanent: the host may have been stranded by them."""
    out = []
    for f in sorted(glob.glob(p(STATE_DIR + "/armed-*.json"))):
        try:
            with open(f) as fh:
                out.append(json.load(fh))
        except (OSError, ValueError):
            out.append({"kernel": os.path.basename(f)[len("armed-"):-len(".json")], "run_id": "unreadable"})
    return out


BOOT_CHAIN_GLOBS = ["grub-*", "grub2-common", "shim*"]


def boot_chain():
    """The installed boot-chain packages as "package=version", byte-sorted, joined by "|". ansible/files/reboot_snapshot.sh
    computes the same string: a rehearsal proves the GRUB and shim that were installed when it ran, no others."""
    rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package}=${Version} ${Status}\n"] + BOOT_CHAIN_GLOBS)
    names = [parts[0] for parts in (line.split() for line in out.splitlines()) if len(parts) >= 2 and parts[-1] == "installed"]
    return "|".join(sorted(names))


def rehearsal_stale(rec, running, proof):
    """Why a rehearsal record no longer counts, or "" when it still does."""
    if not rec:
        return "no rehearsal record"
    if rec.get("kernel") != running or not rec.get("ok"):
        return "the record is for %s, not the running kernel %s" % (rec.get("kernel", "none"), running)
    if not rec.get("chain"):
        return "the record names no boot-chain versions (grub, shim): repeat the rehearsal"
    now = boot_chain()
    if rec["chain"] != now:
        return "grub or shim changed since the rehearsal (then %s; now %s): repeat the rehearsal" % (rec["chain"], now)
    for k in ("efi_loader", "stub_uuid"):
        if rec.get(k, "") != (proof or {}).get(k, ""):
            return "the boot proof changed since the rehearsal (%s was %r, now %r): repeat the rehearsal" % (k, rec.get(k, ""), (proof or {}).get(k, ""))
    return ""


def rehearsal_record():
    try:
        return json.loads(rd(STATE_DIR + "/rehearsal.json") or "{}")
    except ValueError:
        return {}


def rehearsal_with_validity(running, proof):
    rec = rehearsal_record()
    if not rec:
        return {}
    why = rehearsal_stale(rec, running, proof)
    return dict(rec, valid=not why, stale=why)


def marker():
    try:
        return json.loads(rd(STATE_DIR + "/last-upgrade.json") or "{}")
    except ValueError:
        return {}


# ---------------------------------------------------------------------------------------------- survey
NEW_KERNEL_PKG = re.compile(r"^linux-(?:image(?:-unsigned)?|modules(?:-extra)?|headers|tools)-(\d+\.\d+\.\d+-\d+)(?:-\S+)?$")


def new_kernel_pkgs(running, broken_only=False):
    """Installed (or half-installed) kernel packages of a kernel NEWER than the running one. With broken_only: only those of a
    kernel version one of whose packages is not cleanly installed (the version a failed upgrade left behind), with ALL its
    packages: a kernel that was installed before and works is not ours to remove."""
    rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package} ${Status}\n", "linux-image-*", "linux-modules-*", "linux-headers-*", "linux-tools-*"])
    by_version = {}
    armed = {kver_prefix(r.get("kernel", "")) for r in armed_records()}
    for line in out.splitlines():
        parts = line.split()
        if len(parts) < 4 or parts[-1] in ("not-installed", "config-files"):
            continue
        m = NEW_KERNEL_PKG.match(parts[0])
        if m and kver_key(m.group(1) + "-generic") > kver_key(running):
            by_version.setdefault(m.group(1), []).append((parts[0], parts[-1]))
    names = []
    for ver, pk in by_version.items():
        if broken_only and all(st == "installed" for _, st in pk) and ver not in armed:
            continue
        names += [n for n, _ in pk]
    return sorted(set(names))


def access_problems():
    """Read-only checks of the way in (ssh, tailscale, netplan): what BOTH kernels share and a power-cycle cannot undo."""
    probs = []
    if have("sshd"):
        rc, _, err = run(["sshd", "-t"])
        if rc != 0:
            probs.append("sshd -t fails: " + err.strip()[-160:])
    else:
        probs.append("no sshd binary")
    if not any(run(["systemctl", "is-enabled", u])[1].strip() in ("enabled", "static", "alias") for u in ("ssh", "ssh.socket", "sshd")):
        probs.append("neither ssh.service nor ssh.socket is enabled")
    if have("tailscaled"):
        rc, _, err = run(["tailscaled", "--version"])
        if rc != 0:
            probs.append("the installed tailscaled does not run (--version): " + err.strip()[-120:])
        if run(["systemctl", "is-enabled", "tailscaled"])[1].strip() != "enabled":
            probs.append("tailscaled is not enabled")
    if have("netplan") and os.path.isdir(p("/etc/netplan")):
        t = tempfile.mkdtemp(prefix="osu-netplan-")
        try:
            shutil.copytree(p("/etc/netplan"), os.path.join(t, "etc", "netplan"))
            rc, _, err = run(["netplan", "generate", "--root-dir", t])
            if rc != 0:
                probs.append("netplan generate fails on the installed configuration: " + err.strip()[-160:])
        finally:
            shutil.rmtree(t, ignore_errors=True)
    return probs


def blacklist_files():
    out = []
    for f in sorted(glob.glob(p("/etc/modprobe.d/*.conf"))):
        try:
            if re.search(r"^\s*(blacklist|install)\s+r8169\b", open(f, errors="replace").read(), re.M):
                out.append(f)
        except OSError:
            pass
    return out


def survey():
    running = running_kernel()
    kernels = installed_kernels()
    vm = vmlinuz_versions()
    same = [k for k in kernels if flavor(k) == flavor(running)] or [k for k in vm if flavor(k) == flavor(running)]
    boot = boot_info()
    nic_list = nics()
    held = [x for x in run(["apt-mark", "showhold"])[1].split() if x]
    sim = simulate()
    sim_phased = simulate(include_phased=True)
    phased_held = sorted({i["name"] for i in sim_phased["inst"]} - {i["name"] for i in sim["inst"]})
    cfg = apt_config()
    rr = rd("/var/run/reboot-required")
    osr = {}
    for line in rd("/etc/os-release").splitlines():
        k, _, v = line.partition("=")
        if k in ("ID", "VERSION_ID", "VERSION_CODENAME", "PRETTY_NAME", "NAME"):
            osr[k] = v.strip('"')
    ent = boot["entries"]
    boot["running_entry_id"] = entry_id_for(parse_grub_cfg(rd("/boot/grub/grub.cfg")), running)
    ntp = run(["timedatectl", "show", "-p", "NTPSynchronized", "--value"])[1].strip()
    lock_files = locks_held()
    d = {
        "schema": 1,
        "host": rd("/etc/hostname"),
        "os_release": osr,
        "kernel": running,
        "arch": os.uname().machine,
        "kernel_flavor": flavor(running),
        "installed_kernels": kernels,
        "vmlinuz_versions": vm,
        "newest_same_flavor": same[-1] if same else "",
        "boot": boot,
        "packages": {
            "simulation": sim,
            "kept_back": sim["kept_back"],
            "phased_held_back": phased_held,
            "held": held,
            "held_kernel": [h for h in held if h.startswith(("linux-", "grub", "dkms"))],
            "tool_holds": tool_holds(),
            "lists_age_hours": lists_age_hours(),
            "repos": sources(),
            "third_party_repos": sorted({r["uri"] for r in sources() if not r["official"]}),
            "pin_files": sorted(os.path.basename(x) for x in glob.glob(p("/etc/apt/preferences.d/*")) + glob.glob(p("/etc/apt/preferences"))),
            "dpkg_audit": [x for x in run(["dpkg", "--audit"])[1].splitlines() if x.strip()],
            "package_processes": package_processes(),
            "locks_held": lock_files,
            "dpkg_lock_held": bool(lock_files),
            "policy_rc_d_present": os.path.exists(p(POLICY_RC_D)),
            "policy_rc_d_ours": rd(POLICY_RC_D) == POLICY.strip(),
            "new_kernel_pkgs": new_kernel_pkgs(running),
            "broken_kernel_pkgs": new_kernel_pkgs(running, broken_only=True),
        },
        "unattended_upgrades": {
            "installed": bool(dpkg_version("unattended-upgrades")),
            "update_package_lists": cfg.get("APT::Periodic::Update-Package-Lists", ""),
            "unattended_upgrade": cfg.get("APT::Periodic::Unattended-Upgrade", ""),
            "remove_unused_kernel_packages": cfg.get("Unattended-Upgrade::Remove-Unused-Kernel-Packages", ""),
            "automatic_reboot": cfg.get("Unattended-Upgrade::Automatic-Reboot", ""),
            "install_on_shutdown": cfg.get("Unattended-Upgrade::InstallOnShutdown", ""),
            "units": {u: svc(u) for u in ("unattended-upgrades.service", "apt-daily.timer", "apt-daily-upgrade.timer")},
            "needrestart_installed": bool(dpkg_version("needrestart")),
        },
        "reboot_required": bool(rr),
        "reboot_required_pkgs": rd("/var/run/reboot-required.pkgs").split(),
        "dkms": {"status": dkms_modules(), "installed_for_running": dkms_installed_for(running)},
        "access_problems": access_problems(),
        "nics": nic_list,
        "r8127": r8127_report(sorted(set(kernels + ([running] if running else [])), key=kver_key), running, nic_list),
        "firmware": {"linux_firmware": dpkg_version("linux-firmware")},
        "disk": {"/": df("/"), "/boot": df("/boot"), "/boot/efi": df("/boot/efi"), "/var": df("/var")},
        "time_sync": {"ntp_synchronized": ntp == "yes", "raw": ntp},
        "crash": sorted(os.listdir(p("/var/crash"))) if os.path.isdir(p("/var/crash")) else [],
        "host_toolchain": host_toolchain(),
        "marker": marker(),
        "armed_kernels": armed_records(),
        "rehearsal": rehearsal_with_validity(running, boot["proof"]),
        "freeze": freeze_state(),
    }
    return d


# ---------------------------------------------------------------------------------------------- dkms-check
def dkms_check(running, target, build):
    built_now, rc_build, make_logs = False, None, {}
    needed = dkms_installed_for(running)

    def missing():
        have_t = set(dkms_installed_for(target))
        return [m for m in needed if m not in have_t]

    if build and missing() and have("dkms"):
        rc_build, out, err = run(["dkms", "autoinstall", "-k", target], timeout=1800)
        built_now = True
        make_logs["dkms_autoinstall_tail"] = (out + err).splitlines()[-60:]
        for f in glob.glob(p("/var/lib/dkms/*/*/build/make.log")):
            make_logs[f[len(ROOT):]] = open(f, errors="replace").read().splitlines()[-60:]
    nic_list = nics()
    drivers = sorted({n["driver"] for n in nic_list if n["driver"]})
    drv_missing = [d for d in drivers if not modinfo(target, d)["found"] and not builtin_module(target, d)]
    entries = parse_grub_cfg(rd("/boot/grub/grub.cfg"))
    env = grub_editenv()
    ids = sorted({n["pci_id"] for n in nic_list if n["driver"] == "r8127" and n["pci_id"]}) or [PCI_REALTEK + ":8127"]
    r = modinfo(target, "r8127")
    claims = {i: pci_claims(target, i) for i in ids}
    settings, _ = grub_settings()
    boot_sb = boot_info()["secure_boot"]
    return {
        "target": target,
        "running": running,
        "dkms_needed": needed,
        "missing_modules": missing(),
        "built_now": built_now,
        "build_rc": rc_build,
        "make_logs": make_logs,
        "headers_present": os.path.isdir(p("/lib/modules/%s/build" % target)),
        "vmlinuz_present": os.path.exists(p("/boot/vmlinuz-%s" % target)),
        "initrd_present": os.path.exists(p("/boot/initrd.img-%s" % target)),
        "nic_drivers": drivers,
        "nic_drivers_missing": drv_missing,
        "r8127": r,
        "r8127_aliases": bool(run(["modinfo", "-k", target, "-F", "alias", "r8127"])[1].strip()) if r["found"] else False,
        "intree_claims": claims,
        "intree_lists_id": any("r8169" in v for v in claims.values()),
        # does the RUNNING kernel's r8169 list it too? Then the race (if any) exists today and works out as r8127
        "intree_lists_id_running": any("r8169" in pci_claims(running, i) for i in ids),
        "r8169_blacklisted": r8127_blacklisted(),
        # the blacklist only holds in the initramfs when the initramfs was built after the blacklist file was written
        "r8169_blacklist_in_initrd": bool(blacklist_files()) and all(
            os.path.exists(p("/boot/initrd.img-%s" % target)) and os.path.getmtime(p("/boot/initrd.img-%s" % target)) >= os.path.getmtime(f)
            for f in blacklist_files()),
        "running_r8127_signer": modinfo(running, "r8127")["signer"],
        "target_older_than_running": kver_key(target) < kver_key(running),
        "target_armed_before": [r for r in armed_records() if r.get("kernel") == target],
        "access_problems": access_problems(),
        "secure_boot": boot_sb,
        "quiesce_hook": os.access(p("/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown"), os.X_OK),
        "target_entry_id": entry_id_for(entries, target),
        "running_entry_id": entry_id_for(entries, running),
        "saved_entry": env.get("saved_entry", ""),
        "saved_resolves_to": kernel_of_entry(entries, env.get("saved_entry", "")),
        "cfg_uses_saved": 'set default="${saved_entry}"' in rd("/boot/grub/grub.cfg"),
        "grub_savedefault": settings.get("GRUB_SAVEDEFAULT", "") == "true",
        "recordfail_timeout": cfg_recordfail_timeout(rd("/boot/grub/grub.cfg")),
    }


def r8127_blacklisted():
    for f in sorted(glob.glob(p("/etc/modprobe.d/*.conf"))):
        for line in open(f, errors="replace").read().splitlines():
            if re.match(r"^\s*(blacklist|install)\s+r8169\b", line):
                return True
    return False


# ---------------------------------------------------------------------------------------------- changing steps
def fail(msg):
    sys.stderr.write("os_upgrade.py: " + msg + "\n")
    emit({"ok": False, "error": msg})
    sys.exit(1)


def kernel_packages(kernel):
    names = ["linux-image-%s" % kernel, "linux-image-unsigned-%s" % kernel, "linux-modules-%s" % kernel,
             "linux-modules-extra-%s" % kernel, "linux-headers-%s" % kernel, "linux-tools-%s" % kernel]
    rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package} ${Status}\n"] + names)
    return [l.split()[0] for l in out.splitlines() if l.endswith(" installed")]


def unpin():
    """Undo the drop-in of a pin that did not complete (best effort): the grub config goes back to what it was."""
    try:
        os.remove(p(DROPIN))
    except OSError:
        pass
    run(["update-grub"], timeout=600)


def cmd_pin_grub(a):
    running = a.running
    if not running:
        fail("--running is required")
    settings, _ = grub_settings()
    if settings.get("GRUB_SAVEDEFAULT", "") == "true":
        fail("GRUB_SAVEDEFAULT=true: booting a one-shot entry would make it the saved default (nothing was written)")
    d = "%s/%s" % (STATE_DIR, a.run_id)
    try:
        return pin_grub_body(a, running, d)
    except SystemExit:
        unpin()  # a failed pin must not leave GRUB_DEFAULT=saved behind with a stale saved_entry
        raise


def pin_grub_body(a, running, d):
    cur = rd("/etc/default/grub")
    write(d + "/etc-default-grub.bak", cur + "\n")
    # GRUB_RECORDFAIL_TIMEOUT: after a failed boot a headless host must still come up on the saved (old) kernel
    # after a power-cycle, not wait at a menu for ever
    write(DROPIN, "# Managed by probatorium ansible/os-upgrade.yml (run %s): the saved entry is the boot default.\n"
          "GRUB_DEFAULT=saved\nGRUB_RECORDFAIL_TIMEOUT=5\n" % a.run_id)
    rc, out, err = run(["update-grub"], timeout=600)
    if rc != 0:
        fail("update-grub failed: " + (out + err)[-300:])
    entries = parse_grub_cfg(rd("/boot/grub/grub.cfg"))
    cfg = rd("/boot/grub/grub.cfg")
    if 'set default="${saved_entry}"' not in cfg:
        fail("the generated grub.cfg does not use the saved entry (a GRUB_DEFAULT elsewhere overrides the drop-in)")
    rt = cfg_recordfail_timeout(cfg)
    if rt == "-1":
        fail("the generated grub.cfg waits at the menu for ever after a failed boot (recordfail timeout -1)")
    eid = entry_id_for(entries, running)
    if not eid:
        fail("no grub menu entry for the running kernel %s" % running)
    rc, out, err = run(["grub-set-default", eid])
    if rc != 0:
        fail("grub-set-default failed: " + (out + err)[-300:])
    saved = grub_editenv().get("saved_entry", "")
    if saved != eid:
        fail("grub-editenv says saved_entry=%r, expected %r" % (saved, eid))
    pk = kernel_packages(running)
    if pk:
        rc, out, err = run(["apt-mark", "manual"] + pk)
        if rc != 0:
            fail("apt-mark manual failed: " + (out + err)[-300:])
    emit({"ok": True, "running": running, "entry_id": eid, "saved_entry": saved, "marked_manual": pk,
          "backup": d + "/etc-default-grub.bak", "recordfail_timeout": rt})


APT_ENV = {"DEBIAN_FRONTEND": "noninteractive", "NEEDRESTART_MODE": "l", "NEEDRESTART_SUSPEND": "1",
           "UCF_FORCE_CONFFOLD": "1", "APT_LISTCHANGES_FRONTEND": "none"}
POLICY = "#!/bin/sh\n# Managed by probatorium ansible/os-upgrade.yml: no service is started or restarted behind a run.\nexit 101\n"


def cmd_apt_update(a):
    rc, out, err = run(["apt-get", "-o", "DPkg::Lock::Timeout=600", "update"], env=APT_ENV, timeout=1500)
    sys.stdout.write(out + err)
    sys.exit(0 if rc == 0 else 1)


def upgrade_paths(run_id):
    d = "%s/%s" % (LOG_DIR, run_id)
    return d, d + "/apt-upgrade.log", d + "/apt-upgrade.rc"


def start_detached(run_id, subcmd, extra, held=()):
    d, log, rcf = upgrade_paths(run_id)
    if os.path.exists(p(POLICY_RC_D)) and rd(POLICY_RC_D) != POLICY.strip():
        fail("%s exists already and is not ours: not touching it" % POLICY_RC_D)
    if os.path.exists(p(rcf)):
        fail("a run %s was started already" % run_id)
    os.makedirs(p(d), exist_ok=True)
    # ansible.builtin.script runs a temporary copy of this file and deletes it when the task returns: the detached
    # half runs from a copy of its own, in a directory that stays
    keep = "%s/%s/os_upgrade.py" % (STATE_DIR, run_id)
    os.makedirs(os.path.dirname(p(keep)), exist_ok=True)
    shutil.copyfile(os.path.abspath(__file__), p(keep))
    os.chmod(p(keep), 0o700)
    out = open(p(log), "w")
    child = subprocess.Popen([sys.executable, p(keep), subcmd, "--run-id", run_id] + extra,
                             stdout=out, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL, start_new_session=True,
                             cwd="/", env=dict(os.environ))
    write(d + "/apt-upgrade.pid", "%d\n" % child.pid)
    emit({"ok": True, "pid": child.pid, "log": log, "script": keep, "access_packages_held": sorted(held)})


def cmd_upgrade_start(a):
    if os.path.exists(p(POLICY_RC_D)):
        fail("%s exists already: not touching it" % POLICY_RC_D)
    prior = [x for x in rd(upgrade_paths(a.run_id)[0] + "/held.txt").split() if x]
    start_detached(a.run_id, "_upgrade-run", ["--hold"] if a.hold else [], held=(set(prior) | set(access_to_hold())) if a.hold else [])


def cmd_repair_start(a):
    start_detached(a.run_id, "_repair-run", ["--action", a.action])


def kernel_version_installed(prefix):
    """True while any package of the kernel version PREFIX (7.0.0-40) is installed or half-installed."""
    rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package} ${Status}\n", "linux-image-*", "linux-modules-*", "linux-headers-*", "linux-tools-*"])
    for line in out.splitlines():
        parts = line.split()
        if len(parts) >= 4 and parts[-1] not in ("not-installed", "config-files"):
            m = NEW_KERNEL_PKG.match(parts[0])
            if m and m.group(1) == prefix:
                return True
    return False


KERNEL_META = re.compile(r"^linux-(generic|image-generic|headers-generic|tools-generic|crashdump|virtual|image-virtual|headers-virtual)(-hwe[-0-9.]*)?(-64k)?$")


def cmd_repair_run(a):
    """The detached half of mode=repair: dpkg --configure -a (and, for abort-new-kernel, the purge of the kernels newer than
    the running one first), under the same policy-rc.d discipline as the upgrade."""
    d, log, rcf = upgrade_paths(a.run_id)
    rc = 1
    if a.action == "release-holds":
        # no policy-rc.d, no dpkg: apt-mark unhold of what this tool held (held.txt of every run) and nothing else
        try:
            released, left = release_tool_holds()
            if not released and not left:
                sys.stdout.write("nothing to release: no hold of this tool is in place\n")
            # a hold that apt-mark could not release is a failed repair, not "HOLDS RELEASED": the record stays and so does the repair
            rc = 1 if left else 0
        finally:
            write(rcf, "%d\n" % rc)
        return
    try:
        write(POLICY_RC_D, POLICY, 0o755)
        steps = []
        if a.action == "abort-new-kernel":
            pk = new_kernel_pkgs(running_kernel(), broken_only=True)
            if pk:
                common = ["-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "-o", "DPkg::Lock::Timeout=600"]
                # apt removes whatever depends on a package it removes: linux-image-generic and the other kernel metapackages follow
                # the new image. Simulate first; anything outside the set and the metapackages stops the abort.
                rcs, outs, errs = run(["apt-get", "-s", "-o", "Debug::NoLocking=true"] + common[2:] + ["remove", "--purge"] + pk, env=APT_ENV)
                sys.stdout.write("+ apt-get -s remove --purge %s\n%s%s\n" % (" ".join(pk), outs, errs))
                cascade = sorted({m.group(1) for m in (REMV.match(l) for l in outs.splitlines()) if m} - set(pk))
                alien = [c for c in cascade if not KERNEL_META.match(c)]
                metas = [c for c in cascade if KERNEL_META.match(c)]
                if rcs != 0 or alien:
                    sys.stdout.write("REFUSED: the removal would also remove %s (simulation exit %d): nothing was removed\n" % (", ".join(alien) or "(unknown)", rcs))
                    rc = 3
                    pk = []
                    steps = []
                else:
                    if metas:
                        sys.stdout.write("KERNEL METAPACKAGES REMOVED WITH IT: %s (later full-upgrades install no kernel until they are installed again)\n" % ", ".join(metas))
                    steps.append(["apt-get", "-y"] + common + ["remove", "--purge"] + pk)
        if rc == 3:
            return
        steps.append(["dpkg", "--force-confold", "--configure", "-a"])
        purged_versions = {NEW_KERNEL_PKG.match(n).group(1) for n in (pk if a.action == "abort-new-kernel" else []) if NEW_KERNEL_PKG.match(n)}
        rc = 0
        purge_rc = 0
        for argv in steps:
            sys.stdout.write("+ " + " ".join(argv) + "\n")
            sys.stdout.flush()
            r = subprocess.run(argv, env=dict(os.environ, **APT_ENV, LC_ALL="C"), stdout=sys.stdout, stderr=subprocess.STDOUT)
            if "remove" in argv:
                purge_rc = r.returncode
            if r.returncode != 0:
                rc = r.returncode
        # An explicit decision resets the suspicion of a kernel that is GONE: the purge exited 0 AND dpkg no longer shows the version.
        # A failed purge keeps the record: the kernel is still installed and still stranded the host.
        if purge_rc == 0:
            for rec in armed_records():
                pre = kver_prefix(rec.get("kernel", ""))
                if pre in purged_versions and not kernel_version_installed(pre):
                    try:
                        os.remove(p("%s/armed-%s.json" % (STATE_DIR, rec["kernel"])))
                    except OSError:
                        pass
    finally:
        try:
            os.remove(p(POLICY_RC_D))
        except OSError:
            pass
        write(rcf, "%d\n" % rc)


ACCESS_GLOBS = ["grub-*", "grub2-common", "shim*", "openssh-server", "tailscale*"]


def access_packages():
    """Installed packages of the way in and of the bootloader: both kernels share them, so no fallback helps if they break."""
    rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package} ${Status}\n"] + ACCESS_GLOBS)
    return sorted({l.split()[0] for l in out.splitlines() if l.endswith(" installed")})


def access_to_hold():
    """The access-path packages THIS run would hold: not the ones somebody held before."""
    held_before = set(run(["apt-mark", "showhold"])[1].split())
    return [n for n in access_packages() if n not in held_before]


def tool_holds():
    """Holds this tool put (recorded in a run's held.txt) that are still in place: [{run_id, packages}]. A run that died after
    holding leaves them, and they dead-end every later upgrade."""
    held_now = set(run(["apt-mark", "showhold"])[1].split())
    out = []
    for f in sorted(glob.glob(p(LOG_DIR + "/*/held.txt"))):
        run_id = os.path.basename(os.path.dirname(f))
        names = sorted(n for n in rd("%s/%s/held.txt" % (LOG_DIR, run_id)).split() if n in held_now)
        if names:
            out.append({"run_id": run_id, "packages": names})
    return out


def unhold(names):
    """apt-mark unhold NAMES, then ask apt which of them are STILL held: (released, remaining). The exit code of `apt-mark unhold` is not
    believed (it fails, and changes nothing, while unattended-upgrades or PackageKit has the dpkg frontend lock): only what
    `apt-mark showhold` no longer lists is released. If showhold cannot be read, nothing counts as released."""
    names = sorted(set(names))
    if not names:
        return [], []
    run(["apt-mark", "unhold"] + names)
    rc, out, _ = run(["apt-mark", "showhold"])
    if rc != 0:
        return [], names
    still = set(out.split())
    return [n for n in names if n not in still], [n for n in names if n in still]


def keep_record(held_txt, names):
    """The record of the holds this tool put: exactly NAMES (the ones still in place), or no record when none is."""
    if names:
        write(held_txt, " ".join(sorted(names)) + "\n")
        return
    try:
        os.remove(p(held_txt))
    except OSError:
        pass


def prune_hold_records():
    """A record names holds that are no longer in place (somebody unheld them by hand): drop those names, so that a hold put later
    by a person is never claimed by a dead run's record (and released by it)."""
    rc, out, _ = run(["apt-mark", "showhold"])
    if rc != 0:
        return  # cannot tell what is held: every record stays
    held_now = set(out.split())
    for f in sorted(glob.glob(p(LOG_DIR + "/*/held.txt"))):
        rel = f[len(ROOT):] if ROOT else f
        names = [n for n in rd(rel).split() if n]
        if [n for n in names if n in held_now] != names:
            keep_record(rel, [n for n in names if n in held_now])


def release_tool_holds():
    """Releases every hold this tool put and records it in the output. Returns ([{run_id, packages}] released, [{run_id, packages}] still
    held). Never touches a hold that is not in a held.txt (somebody else's). A hold that is still in place afterwards keeps its record
    (so the next survey and the next upgrade still name the repair) and is reported as NOT released."""
    released, left = [], []
    for rec in tool_holds():
        done, remaining = unhold(rec["packages"])
        sys.stdout.write("+ apt-mark unhold %s\n" % " ".join(rec["packages"]))
        if done:
            sys.stdout.write("HOLDS RELEASED: %s (put by run %s)\n" % (" ".join(done), rec["run_id"]))
            released.append({"run_id": rec["run_id"], "packages": done})
        if remaining:
            sys.stdout.write("HOLDS NOT RELEASED: %s (put by run %s): still held after apt-mark unhold, the record is kept\n" % (" ".join(remaining), rec["run_id"]))
            left.append({"run_id": rec["run_id"], "packages": remaining})
    prune_hold_records()  # the records of holds that are gone (or were just released) go; the ones still in place stay
    return released, left


def cmd_hold_access(a):
    """Holds the access-path packages now, before the plan is simulated, and records them for this run."""
    d, log, rcf = upgrade_paths(a.run_id)
    prune_hold_records()
    prior = [x for x in rd(d + "/held.txt").split() if x]
    new = access_to_hold()
    os.makedirs(p(d), exist_ok=True)
    write(d + "/held.txt", " ".join(sorted(set(prior) | set(new))) + "\n")
    if new:
        rc, out, err = run(["apt-mark", "hold"] + new)
        if rc != 0:
            unhold_ours(a.run_id)
            fail("apt-mark hold failed: " + (out + err)[-300:])
    emit({"ok": True, "held": new, "recorded": sorted(set(prior) | set(new))})


def cmd_release_access(a):
    released, left = unhold_ours(a.run_id)
    if left:
        msg = "the access-path holds %s could NOT be released (apt-mark unhold did not take: the dpkg lock is busy?); the record is kept: dispatch mode=repair with repair_action=release-holds" % " ".join(left)
        sys.stderr.write("os_upgrade.py: " + msg + "\n")
        emit({"ok": False, "released": released, "still_held": left, "error": msg})
        sys.exit(1)
    emit({"ok": True, "released": released, "still_held": []})


def unhold_ours(run_id):
    """Releases the holds THIS run put (never one that was there before). Returns (released, still_held): a hold that apt-mark could not
    release (see unhold) stays in the run's record, so it is found and named later."""
    d, log, rcf = upgrade_paths(run_id)
    names = [x for x in rd(d + "/held.txt").split() if x]
    if not names:
        return [], []
    released, remaining = unhold(names)
    keep_record(d + "/held.txt", remaining)
    return released, remaining


def cmd_upgrade_run(a):
    """The detached half: runs apt-get with the policy-rc.d in place and removes it whatever happens."""
    d, log, rcf = upgrade_paths(a.run_id)
    rc = 1
    try:
        if a.hold:
            # the holds were normally put by hold-access, before the simulation; whatever is not held yet is held now
            prior = [x for x in rd(d + "/held.txt").split() if x]
            new = access_to_hold()
            write(d + "/held.txt", " ".join(sorted(set(prior) | set(new))) + "\n")
            if new:
                sys.stdout.write("+ apt-mark hold %s\n" % " ".join(new))
                run(["apt-mark", "hold"] + new)
        write(POLICY_RC_D, POLICY, 0o755)
        argv = ["apt-get", "-y", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold",
                "-o", "Dpkg::Use-Pty=0", "-o", "DPkg::Lock::Timeout=600", "full-upgrade"]
        sys.stdout.write("+ " + " ".join(argv) + "\n")
        sys.stdout.flush()
        r = subprocess.run(argv, env=dict(os.environ, **APT_ENV, LC_ALL="C"), stdout=sys.stdout, stderr=subprocess.STDOUT)
        rc = r.returncode
    finally:
        try:
            os.remove(p(POLICY_RC_D))
        except OSError:
            pass
        # the apt run is over: the holds this run put are released, and a hold apt-mark could not release is said in the log (the
        # record stays, upgrade-cleanup tries again and fails the run if it is still there)
        _, left = unhold_ours(a.run_id)
        if left:
            sys.stdout.write("HOLDS NOT RELEASED: %s (put by run %s): still held after apt-mark unhold, the record is kept\n" % (" ".join(left), a.run_id))
        write(rcf, "%d\n" % rc)


def pid_alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    # a zombie (a finished child nobody waited for) is not running
    st = rd("/proc/%d/stat" % pid) if not ROOT else ""
    return not (st.split(") ")[-1:] or [""])[0].startswith("Z")


def cmd_upgrade_status(a):
    d, log, rcf = upgrade_paths(a.run_id)
    deadline = time.time() + max(0, a.wait)
    while True:
        if os.path.exists(p(rcf)):
            emit({"state": "done", "rc": int(rd(rcf) or "1")})
            return
        if not os.path.exists(p(log)):
            emit({"state": "none", "rc": None})
            return
        pid = int(rd(d + "/apt-upgrade.pid") or "0")
        if pid and not pid_alive(pid):
            emit({"state": "died", "rc": None})
            return
        if time.time() >= deadline:
            emit({"state": "running", "rc": None})
            return
        time.sleep(1)


def cmd_upgrade_cleanup(a):
    """Removes OUR policy-rc.d if the detached run is over (finished, or its process is gone) and did not remove it. Never
    while the apt run is alive: a lost poll must not let the services restart under it. Never one that is not ours."""
    d, log, rcf = upgrade_paths(a.run_id)
    pid = int(rd(d + "/apt-upgrade.pid") or "0")
    if not os.path.exists(p(rcf)) and pid and pid_alive(pid):
        emit({"removed": False, "reason": "the apt run is still alive"})
        return
    # the detached wrapper can die (the OOM killer) while its apt-get child, reparented to init, still runs
    busy = package_run_busy()
    if busy:
        emit({"removed": False, "reason": "dpkg or apt is still running (%s)" % busy})
        return
    unheld, left = unhold_ours(a.run_id)  # a run that died with its holds on
    prune_hold_records()
    removed = rd(POLICY_RC_D) == POLICY.strip()
    if removed:
        os.remove(p(POLICY_RC_D))
    res = {"removed": removed, "unheld": unheld, "still_held": left}
    if not removed:
        res["reason"] = "no policy-rc.d of ours"
    if left:
        # the run must not go on to a reboot with the access path held, and the record stays: the next survey lists it
        msg = "the access-path holds %s of run %s could NOT be released (apt-mark unhold did not take: the dpkg lock is busy?); the record is kept and nothing is rebooted: dispatch mode=repair with repair_action=release-holds" % (" ".join(left), a.run_id)
        sys.stderr.write("os_upgrade.py: " + msg + "\n")
        emit(dict(res, ok=False, error=msg))
        sys.exit(1)
    emit(res)


def cmd_upgrade_log(a):
    d, log, rcf = upgrade_paths(a.run_id)
    sys.stdout.write(rd(log) + "\n")


def cmd_blacklist(a):
    write(MODPROBE_CONF, "# Managed by probatorium ansible/os-upgrade.yml (run %s): keep the out-of-tree r8127 bound to the "
          "RTL8127; the in-tree r8169 of newer kernels lists the same PCI id.\nblacklist r8169\n" % a.run_id)
    rc, out, err = run(["update-initramfs", "-u", "-k", a.target], timeout=900)
    if rc != 0:
        fail("update-initramfs -u -k %s failed: %s" % (a.target, (out + err)[-300:]))
    emit({"ok": True, "file": MODPROBE_CONF})


def cmd_dkms_check(a):
    emit(dkms_check(a.running, a.target, a.build))


def current_boot_id():
    return rd("/proc/sys/kernel/random/boot_id").strip()


def forget_armed_without_a_reboot():
    """An armed record says "this kernel was armed and no run verified the host on it": the suspicion that it stranded the host. A
    record made in THIS boot (same boot id) cannot have stranded it: the host has not rebooted since. Called after a successful
    disarm; returns the kernels whose record was dropped."""
    bid = current_boot_id()
    gone = []
    if not bid:
        return gone
    for rec in armed_records():
        if rec.get("boot_id") == bid and rec.get("kernel"):
            try:
                os.remove(p("%s/armed-%s.json" % (STATE_DIR, rec["kernel"])))
                gone.append(rec["kernel"])
            except OSError:
                pass
    return gone


def disarm():
    """Clears a pending one-shot (grub's next_entry). Returns the JSON of what it found and did. A kernel armed in this very boot
    never booted: its armed record is dropped (forgot_armed), so the next upgrade is not refused for a boot that never happened."""
    env = grub_editenv()
    before = env.get("next_entry", "")
    rc, out, err = 0, "", ""
    if before:
        rc, out, err = run(["grub-editenv", "-", "unset", "next_entry"])
    after = grub_editenv().get("next_entry", "")
    ok = rc == 0 and not after
    return {"ok": ok, "was_armed": bool(before), "next_entry_before": before, "next_entry_after": after,
            "saved_entry": env.get("saved_entry", ""), "error": (out + err)[-300:] if rc != 0 else "",
            "forgot_armed": forget_armed_without_a_reboot() if ok else []}


def cmd_disarm(a):
    r = disarm()
    emit(r)
    sys.exit(0 if r["ok"] else 1)


def cmd_arm(a):
    if not a.run_id:
        fail("--run-id is required")
    # an unattended upgrade that started since the check would be cut by the reboot
    busy = package_run_busy()
    if busy:
        fail("dpkg or apt is busy (%s): not arming a reboot into a running package run" % busy)
    rc, out, err = run(["update-grub"], timeout=600)
    if rc != 0:
        fail("update-grub failed: " + (out + err)[-300:])
    entries = grub_entries()
    old, new = entry_id_for(entries, a.running), entry_id_for(entries, a.target)
    if not old or not new:
        fail("no grub entry for %s (%r) or %s (%r)" % (a.running, old, a.target, new))
    rc, out, err = run(["grub-set-default", old])
    if rc != 0:
        fail("grub-set-default failed: " + (out + err)[-300:])
    rc, out, err = run(["grub-reboot", new])
    if rc != 0:
        fail("grub-reboot failed: " + (out + err)[-300:])
    env = grub_editenv()
    if env.get("saved_entry") != old or env.get("next_entry") != new:
        d = disarm()  # never leave an unverified one-shot behind
        fail("grub-editenv: saved_entry=%r next_entry=%r, expected %r and %r; one-shot disarmed: %s" % (
            env.get("saved_entry"), env.get("next_entry"), old, new, d["ok"]))
    write("%s/armed-%s.json" % (STATE_DIR, a.target), json.dumps({"kernel": a.target, "running": a.running, "run_id": a.run_id, "boot_id": current_boot_id(),
          "armed_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}, sort_keys=True) + "\n")
    emit({"ok": True, "saved_entry": old, "next_entry": new, "running": a.running, "target": a.target})


def cmd_arm_rehearsal(a):
    """Arms next_entry to a boot of the RUNNING kernel through its own Advanced entry, the entry id that names this kernel only
    (a top-level id such as gnulinux-simple-<uuid> means "the newest kernel at boot time": a kernel installed between the arming
    and the reboot would be booted). It proves that GRUB reads the environment block Linux writes (next_entry is empty after the
    boot). NOT a zero-kernel-risk test: if GRUB ignores the block it boots its first menu entry (see boot.entry0_kernel), which is
    another kernel while a newer one is installed; the judge refuses that unless the maintainer confirmed a console."""
    busy = package_run_busy()
    if busy:
        fail("dpkg or apt is busy (%s): not arming a reboot into a running package run" % busy)
    rc, out, err = run(["update-grub"], timeout=600)
    if rc != 0:
        fail("update-grub failed: " + (out + err)[-300:])
    entries = grub_entries()
    saved_id = entry_id_for(entries, a.running)
    if not saved_id:
        fail("no grub entry for %s" % a.running)
    env = grub_editenv()
    if env.get("saved_entry") != saved_id:
        fail("the saved default is %r, not the running kernel's entry %r: pin it first" % (env.get("saved_entry"), saved_id))
    new = saved_id
    rc, out, err = run(["grub-reboot", new])
    if rc != 0:
        fail("grub-reboot failed: " + (out + err)[-300:])
    env = grub_editenv()
    if env.get("saved_entry") != saved_id or env.get("next_entry") != new:
        d = disarm()
        fail("grub-editenv: saved_entry=%r next_entry=%r, expected %r and %r; one-shot disarmed: %s" % (
            env.get("saved_entry"), env.get("next_entry"), saved_id, new, d["ok"]))
    emit({"ok": True, "saved_entry": saved_id, "next_entry": new, "running": a.running, "target": a.running, "via": "advanced entry"})


def cmd_record_rehearsal(a):
    if running_kernel() != a.kernel:
        fail("the host runs %s, not %s" % (running_kernel(), a.kernel))
    if grub_editenv().get("next_entry"):
        fail("a one-shot is still pending: GRUB did not consume it")
    proof = boot_info()["proof"]
    rec = {"kernel": a.kernel, "run_id": a.run_id, "ok": True, "chain": boot_chain(), "efi_loader": proof.get("efi_loader", ""),
           "stub_uuid": proof.get("stub_uuid", ""), "utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    write(STATE_DIR + "/rehearsal.json", json.dumps(rec, sort_keys=True) + "\n")
    emit(rec)


def cmd_make_permanent(a):
    if running_kernel() != a.target:
        fail("the host runs %s, not %s: refusing to make an unbooted kernel the default" % (running_kernel(), a.target))
    entries = grub_entries()
    eid = entry_id_for(entries, a.target)
    if not eid:
        fail("no grub entry for %s" % a.target)
    pending = grub_editenv().get("next_entry", "")
    if pending:
        # GRUB booted the one-shot and did not consume it: it reads another environment block than the one Linux writes
        fail("a one-shot is still pending (%r) after the new kernel booted: GRUB did not consume it, so the fallback is not proven; nothing was changed" % pending)
    rc, out, err = run(["grub-set-default", eid])
    if rc != 0:
        fail("grub-set-default failed: " + (out + err)[-300:])
    env = grub_editenv()
    if env.get("saved_entry") != eid or env.get("next_entry"):
        fail("grub-editenv: saved_entry=%r next_entry=%r, expected %r and no next_entry (the saved default is NOW %r)" % (
            env.get("saved_entry"), env.get("next_entry"), eid, env.get("saved_entry")))
    rec = {"run_id": a.run_id, "host": rd("/etc/hostname"), "kernel_before": a.running, "kernel_after": a.target,
           "entry_id": eid, "permanent": True, "finished_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    write(STATE_DIR + "/last-upgrade.json", json.dumps(rec, indent=1, sort_keys=True) + "\n")
    try:
        os.remove(p("%s/armed-%s.json" % (STATE_DIR, a.target)))  # verified: no longer a suspect
    except OSError:
        pass
    emit({"ok": True, **rec})


def cmd_toolcheck(a):
    """A cheap look at whether the bootstrap's prerequisites work on the upgraded host: the system C compiler builds and
    runs a program, TLS to GitHub (the runner tarball) verifies, and the other download hosts of the roles answer."""
    res = {"cc_ok": False, "https_ok": False, "endpoints": {}, "warnings": []}
    cc = shutil.which("cc") or shutil.which("gcc")
    t = tempfile.mkdtemp(prefix="osu-toolcheck-")
    try:
        if cc:
            src = os.path.join(t, "t.c")
            open(src, "w").write("#include <stdio.h>\nint main(void){puts(\"ok\");return 0;}\n")
            rc, _, _ = run([cc, src, "-o", os.path.join(t, "t")], timeout=120, check_ro=False)
            rc2, out, _ = run([os.path.join(t, "t")], check_ro=False) if rc == 0 else (1, "", "")
            res["cc_ok"] = rc == 0 and rc2 == 0 and out.strip() == "ok"
        else:
            res["warnings"].append("no cc or gcc on the host")
        for host, hard in (("github.com", True), ("nodejs.org", False), ("static.rust-lang.org", False), ("astral.sh", False),
                           ("api.adoptium.net", False), ("ziglang.org", False)):
            rc, _, _ = run(["curl", "-fsSI", "--max-time", "20", "-o", "/dev/null", "https://" + host], timeout=40, check_ro=False)
            res["endpoints"][host] = rc == 0
            if hard:
                res["https_ok"] = rc == 0
            elif rc != 0:
                res["warnings"].append("https://%s did not answer (curl rc %d)" % (host, rc))
    finally:
        shutil.rmtree(t, ignore_errors=True)
    emit(res)


def cmd_dump(a):
    if a.what == "packages":
        rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package}\t${Version}\t${Architecture}\t${db:Status-Abbrev}\n"])
        sys.stdout.write("".join(sorted(out.splitlines(True))))
    elif a.what == "policy":
        sys.stdout.write(run(["apt-cache", "policy"])[1])
        sys.stdout.write("\n# held: %s\n" % " ".join(run(["apt-mark", "showhold"])[1].split()))
    elif a.what == "sysctl":
        sys.stdout.write("".join(sorted(run(["sysctl", "-a"])[1].splitlines(True))))
    elif a.what == "cmdline":
        sys.stdout.write(rd("/proc/cmdline") + "\n")
    elif a.what == "dpkg-l":
        sys.stdout.write(run(["dpkg", "-l"])[1])
    elif a.what == "held":
        sys.stdout.write("\n".join(sorted(x for x in run(["apt-mark", "showhold"])[1].split() if x)) + "\n")
    elif a.what == "kernel":
        osr = {}
        for line in rd("/etc/os-release").splitlines():
            k, _, v = line.partition("=")
            if k in ("PRETTY_NAME", "VERSION_ID"):
                osr[k] = v.strip('"')
        sys.stdout.write("os: %s\nrunning_kernel: %s\ninstalled_kernels: %s\nnewest_same_flavor: %s\ncmdline: %s\nboot_id: %s\n" % (
            osr.get("PRETTY_NAME", ""), running_kernel(), " ".join(installed_kernels()),
            ([k for k in installed_kernels() if flavor(k) == flavor(running_kernel())] or [""])[-1], rd("/proc/cmdline"), current_boot_id()))
    elif a.what == "grub":
        env = grub_editenv()
        sys.stdout.write("saved_entry=%s\nnext_entry=%s\n" % (env.get("saved_entry", ""), env.get("next_entry", "")))
        for e in grub_entries():
            sys.stdout.write("%s\t%s\t%s\n" % (e["kernel"], e["full_id"], "recovery" if e["recovery"] else ""))


# ---------------------------------------------------------------------------------------------- freeze / thaw
# A 131-hour benchmark must not see a package or a service change under it, and these hosts update themselves: unattended-upgrades
# installed the kernel -38 on its own. `freeze` stops the platform from moving by itself (the apt timers and unattended-upgrades, and
# needrestart's automatic service restarts) and records the platform it froze; `thaw` puts back exactly what freeze recorded it changed.
FREEZE_UNITS = ("apt-daily.timer", "apt-daily-upgrade.timer", "unattended-upgrades.service")
FREEZE_RECORD = STATE_DIR + "/freeze.json"
NEEDRESTART_DROPIN = "/etc/needrestart/conf.d/zzz-celeris-freeze.conf"
NEEDRESTART_TEXT = ("# Managed by probatorium ansible/os-upgrade.yml (mode=freeze): needrestart lists the services that need a restart and restarts NONE, so the\n"
                    "# platform stays fixed under a measurement. mode=thaw removes this file.\n$nrconf{restart} = 'l';\n")


def unit_state(unit):
    st = svc(unit)
    # a unit this host does not have: is-active says inactive and is-enabled says not-found (or prints nothing)
    st["present"] = st["enabled"] not in ("not-found", "")
    return st


def needrestart_mode():
    """The restart mode needrestart is configured with ('a' automatic, 'l' list only, 'i' interactive), "" when it is not set anywhere
    (needrestart's own default). Later files win: needrestart.conf, then conf.d/*.conf in name order."""
    mode = ""
    for f in [p("/etc/needrestart/needrestart.conf")] + sorted(glob.glob(p("/etc/needrestart/conf.d/*.conf"))):
        try:
            text = open(f, errors="replace").read()
        except OSError:
            continue
        for m in re.finditer(r"^\s*\$nrconf\{restart\}\s*=\s*['\"]?([a-z])['\"]?\s*;", text, re.M):
            mode = m.group(1)
    return mode


def platform_identity():
    """What the release results can cite as THE platform: the kernel, the OS, the held packages and a hash of every installed package and
    version (the full list is in the evidence: dpkg-l.txt)."""
    import hashlib
    rc, out, _ = run(["dpkg-query", "-W", "-f", "${Package}\t${Version}\t${Architecture}\t${db:Status-Abbrev}\n"])
    lines = sorted(out.splitlines())
    osr = {}
    for line in rd("/etc/os-release").splitlines():
        k, _, v = line.partition("=")
        if k in ("PRETTY_NAME", "VERSION_ID"):
            osr[k] = v.strip('"')
    return {"kernel": running_kernel(), "os": osr.get("PRETTY_NAME", ""), "packages": len(lines),
            "dpkg_sha256": hashlib.sha256("\n".join(lines).encode()).hexdigest(),
            "held": sorted(x for x in run(["apt-mark", "showhold"])[1].split() if x)}


def freeze_record():
    try:
        return json.loads(rd(FREEZE_RECORD) or "{}")
    except ValueError:
        return {"status": "unreadable"}


def freeze_state():
    """The freeze record and what it says about NOW: for every unit it changed, is it still stopped and disabled? `drifted` names those that
    are not (something re-enabled them: a package upgrade, a person), so the survey shows a freeze that no longer holds."""
    rec = freeze_record()
    if not rec:
        return {}
    drifted = []
    for u, info in sorted(rec.get("units", {}).items()):
        if not info.get("present"):
            continue  # a unit this host does not have cannot come back
        now = unit_state(u)  # every unit of the freeze, whether it changed it or found it off already: none may run
        if now["active"] == "active" or now["enabled"] == "enabled":
            drifted.append("%s=%s/%s" % (u, now["active"], now["enabled"]))
    nr = rec.get("needrestart", {})
    if nr.get("dropin_written") and needrestart_mode() != "l":
        drifted.append("needrestart restart mode is %r, not 'l'" % needrestart_mode())
    return dict(rec, drifted=drifted)


def cmd_freeze(a):
    if not a.run_id:
        fail("--run-id is required")
    rec = freeze_record()
    if rec:
        fail("this host is already frozen (run %s, %s, status %s): mode=thaw first. Its record is kept, so the previous states it holds are not overwritten" % (
            rec.get("run_id", "?"), rec.get("utc", "?"), rec.get("status", "?")))
    busy = package_run_busy()
    if busy:
        fail("dpkg or apt is busy (%s): not stopping unattended-upgrades or the apt timers under a package run" % busy)
    ios = apt_config().get("Unattended-Upgrade::InstallOnShutdown", "")
    if ios.lower() in ("true", "1", "yes", "on"):
        fail("Unattended-Upgrade::InstallOnShutdown is %r: the shutdown listener (unattended-upgrade-shutdown, which this freeze stops) installs packages "
             "when the host shuts down, and what stopping it does then is not known here: set it to false by a reviewed change first (nothing was changed)" % ios)
    before = {u: unit_state(u) for u in FREEZE_UNITS}
    nr_before = needrestart_mode() if dpkg_version("needrestart") else None
    rec = {"run_id": a.run_id, "host": rd("/etc/hostname"), "utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "status": "freezing",
           "units": {u: {"before": {"active": s["active"], "enabled": s["enabled"]}, "present": s["present"], "changes": []} for u, s in before.items()},
           "needrestart": {"installed": nr_before is not None, "mode_before": nr_before or "", "dropin_written": False},
           "install_on_shutdown": ios}
    # the record is written BEFORE anything changes: a freeze that fails half way can still be thawed
    write(FREEZE_RECORD, json.dumps(rec, indent=1, sort_keys=True) + "\n")
    errors = []
    for u, s in before.items():
        if not s["present"]:
            continue
        if s["active"] == "active":
            rc, out, err = run(["systemctl", "stop", u], timeout=120)
            if rc != 0:
                errors.append("systemctl stop %s: %s" % (u, (out + err).strip()[-160:]))
            else:
                rec["units"][u]["changes"].append("stopped")
                write(FREEZE_RECORD, json.dumps(rec, indent=1, sort_keys=True) + "\n")  # every change is recorded as it is made
        if s["enabled"] == "enabled":
            rc, out, err = run(["systemctl", "disable", u], timeout=120)
            if rc != 0:
                errors.append("systemctl disable %s: %s" % (u, (out + err).strip()[-160:]))
            else:
                rec["units"][u]["changes"].append("disabled")
                write(FREEZE_RECORD, json.dumps(rec, indent=1, sort_keys=True) + "\n")
    if nr_before is not None and nr_before != "l":
        try:
            write(NEEDRESTART_DROPIN, NEEDRESTART_TEXT)
            rec["needrestart"]["dropin_written"] = True
            write(FREEZE_RECORD, json.dumps(rec, indent=1, sort_keys=True) + "\n")
        except OSError as x:
            errors.append("cannot write %s: %s" % (NEEDRESTART_DROPIN, x))
    after = {u: unit_state(u) for u in FREEZE_UNITS}
    for u, s in after.items():
        if s["present"] and (s["active"] == "active" or s["enabled"] == "enabled"):
            errors.append("%s is still %s/%s after the freeze" % (u, s["active"], s["enabled"]))
    if rec["needrestart"]["installed"] and needrestart_mode() != "l":
        errors.append("needrestart is still configured with restart mode %r" % needrestart_mode())
    rec["after"] = {u: {"active": s["active"], "enabled": s["enabled"]} for u, s in after.items()}
    rec["platform"] = platform_identity()
    rec["status"] = "frozen" if not errors else "freezing-failed"
    rec["errors"] = errors
    write(FREEZE_RECORD, json.dumps(rec, indent=1, sort_keys=True) + "\n")
    if errors:
        fail("the freeze did not complete (%s); the record is kept: mode=thaw restores what it changed" % "; ".join(errors))
    emit(dict(rec, ok=True))


def cmd_thaw(a):
    if not a.run_id:
        fail("--run-id is required")
    rec = freeze_record()
    if not rec or rec.get("status") == "unreadable":
        fail("nothing to thaw: no readable freeze record at %s (this tool changed nothing, or the record is gone: put the timers back by hand and say so)" % FREEZE_RECORD)
    errors, done = [], []
    for u, info in sorted(rec.get("units", {}).items()):
        for change in reversed(info.get("changes", [])):  # only what freeze recorded that it changed, in reverse
            verb = {"disabled": "enable", "stopped": "start"}.get(change)
            if not verb:
                continue
            rc, out, err = run(["systemctl", verb, u], timeout=120)
            if rc != 0:
                errors.append("systemctl %s %s: %s" % (verb, u, (out + err).strip()[-160:]))
            else:
                done.append("%s %s" % (verb, u))
        now = unit_state(u)
        want = info.get("before", {})
        if info.get("changes") and (now["active"] != want.get("active") or now["enabled"] != want.get("enabled")):
            errors.append("%s is %s/%s, was %s/%s before the freeze" % (u, now["active"], now["enabled"], want.get("active"), want.get("enabled")))
    nr = rec.get("needrestart", {})
    if nr.get("dropin_written"):
        try:
            if rd(NEEDRESTART_DROPIN) == NEEDRESTART_TEXT.strip():
                os.remove(p(NEEDRESTART_DROPIN))
                done.append("removed " + NEEDRESTART_DROPIN)
            elif os.path.exists(p(NEEDRESTART_DROPIN)):
                errors.append("%s is no longer the file the freeze wrote: left alone" % NEEDRESTART_DROPIN)
        except OSError as x:
            errors.append("cannot remove %s: %s" % (NEEDRESTART_DROPIN, x))
        if needrestart_mode() != nr.get("mode_before", ""):
            errors.append("needrestart restart mode is %r, was %r before the freeze" % (needrestart_mode(), nr.get("mode_before", "")))
    if errors:
        fail("the thaw did not complete (%s); the freeze record is kept: run mode=thaw again" % "; ".join(errors))
    rec["status"] = "thawed"
    rec["thawed_utc"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    rec["thawed_by_run"] = a.run_id
    rec["restored"] = done
    write("%s/freeze-%s-thawed.json" % (STATE_DIR, rec.get("run_id", "x")), json.dumps(rec, indent=1, sort_keys=True) + "\n")
    os.remove(p(FREEZE_RECORD))
    emit(dict(rec, ok=True))


# ---------------------------------------------------------------------------------------------- main
def main():
    global READ_ONLY
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    def add(name, fn, *args, flags=()):
        s = sub.add_parser(name)
        for x in args:
            s.add_argument("--" + x, default="")
        for x in flags:
            s.add_argument("--" + x, action="store_true")
        s.set_defaults(fn=fn)
        return s

    add("survey", lambda a: emit(survey()))
    add("simulate", lambda a: emit(simulate()))
    add("marker", lambda a: emit(marker()))
    s = add("dump", cmd_dump)
    s.add_argument("what", choices=["packages", "policy", "sysctl", "cmdline", "grub", "dpkg-l", "held", "kernel"])
    st = add("upgrade-status", cmd_upgrade_status, "run-id")
    st.add_argument("--wait", type=int, default=0)
    add("upgrade-cleanup", cmd_upgrade_cleanup, "run-id")
    rs = add("repair-start", cmd_repair_start, "run-id")
    rs.add_argument("--action", choices=["configure", "abort-new-kernel", "release-holds"], default="configure")
    rr = add("_repair-run", cmd_repair_run, "run-id")
    rr.add_argument("--action", choices=["configure", "abort-new-kernel", "release-holds"], default="configure")
    add("upgrade-log", cmd_upgrade_log, "run-id")
    add("apt-update", cmd_apt_update)
    add("pin-grub", cmd_pin_grub, "running", "run-id")
    add("hold-access", cmd_hold_access, "run-id")
    add("release-access", cmd_release_access, "run-id")
    add("upgrade-start", cmd_upgrade_start, "run-id", flags=("hold",))
    add("_upgrade-run", cmd_upgrade_run, "run-id", flags=("hold",))
    add("blacklist-r8169", cmd_blacklist, "run-id", "target")
    add("dkms-check", cmd_dkms_check, "running", "target", flags=("build",))
    add("arm-oneshot", cmd_arm, "running", "target", "run-id")
    add("arm-rehearsal", cmd_arm_rehearsal, "running", "target", "run-id")
    add("record-rehearsal", cmd_record_rehearsal, "kernel", "run-id")
    add("disarm", cmd_disarm)
    add("make-permanent", cmd_make_permanent, "running", "target", "run-id")
    add("toolcheck", cmd_toolcheck)
    add("freeze", cmd_freeze, "run-id")
    add("thaw", cmd_thaw, "run-id")
    a = ap.parse_args()
    # survey, simulate, dump, marker and the status reads: nothing may be written or changed. dkms-check without
    # --build is a read too (with it, it runs `dkms autoinstall`).
    if a.cmd in ("survey", "simulate", "dump", "upgrade-status", "upgrade-log", "marker") or (a.cmd == "dkms-check" and not a.build):
        READ_ONLY = True
    a.fn(a)


if __name__ == "__main__":
    main()
