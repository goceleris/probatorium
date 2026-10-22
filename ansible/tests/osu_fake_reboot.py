#!/usr/bin/env python3
"""Stands in for the machine restarting in the tests of ansible/os-upgrade.yml: what grub would boot (the one-shot
entry if next_entry is set, which grub then clears; else the saved entry) becomes the running kernel, the boot id
changes and the interrupt counters reset. BOOT_MODE: keep-next (GRUB boots the saved entry and never clears next_entry: it reads another environment block) | not-issued (the reboot never happens, the host stays up) | ok | ignore-next (the new kernel fails early: grub falls back
to the saved entry) | no-nic (the new kernel boots without enp1s0) | hang (the host never answers: exit 1) | keep-next-reported-failed (it rebooted, grub kept next_entry, and the module gave up) | booted-but-reported-failed | ignore-env (GRUB reads NO environment block: it boots the FIRST top-level menu entry, saved_entry and next_entry both ignored, and never clears next_entry) | ignore-env-gaveup (the same, and the module gave up).

    osu_fake_reboot.py ROOT STATE_DIR
"""
import json
import os
import sys
import uuid

here = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(here, "..", "files"))
root, state_dir = sys.argv[1], sys.argv[2]
os.environ["OSU_ROOT"] = root
import os_upgrade as osu  # noqa: E402

osu.ROOT = root
mode = os.environ.get("BOOT_MODE", "ok")
if mode == "not-issued":
    sys.stderr.write("the reboot was not issued (test): the host is up on its old boot\n")
    sys.exit(1)
with open(os.path.join(state_dir, "argv.log"), "a") as f:
    f.write("FAKE-REBOOT\n")
if mode == "hang":
    open(os.path.join(state_dir, "down"), "w").close()  # every later command on the host fails, as on a dead host
    sys.stderr.write("the host did not come back (test)\n")
    sys.exit(1)
sp = os.path.join(state_dir, "state.json")
if mode == "stale-first":
    # the host answers ssh before it has finished booting: the first snapshots still show the OLD boot
    import shutil
    shutil.rmtree(os.path.join(state_dir, "stale_root"), ignore_errors=True)
    shutil.copytree(root, os.path.join(state_dir, "stale_root"), symlinks=True)
    with open(os.path.join(state_dir, "stale_count"), "w") as f:
        f.write("1\n")
with open(sp) as f:
    st = json.load(f)
if os.environ.get("ENTRY0_AFTER"):
    # grub's first menu entry is ANOTHER kernel than the one the upgrade targeted (an HWE or newer-flavor kernel sorts first)
    with open(root + "/boot/grub/grub.cfg") as f:
        cfg_text = f.read()
    i = cfg_text.index("vmlinuz-") + len("vmlinuz-")
    j = i
    while cfg_text[j] not in " \t\n":
        j += 1
    with open(root + "/boot/grub/grub.cfg", "w") as f:
        f.write(cfg_text[:i] + os.environ["ENTRY0_AFTER"] + cfg_text[j:])
with open(root + "/boot/grub/grub.cfg") as f:
    entries = osu.parse_grub_cfg(f.read())
env = st["grubenv"]
ref = env.get("saved_entry", "")
if env.get("next_entry") and mode not in ("ignore-next", "keep-next", "keep-next-reported-failed", "ignore-env", "ignore-env-gaveup"):
    ref = env["next_entry"]
if mode not in ("keep-next", "keep-next-reported-failed", "ignore-env", "ignore-env-gaveup"):
    env["next_entry"] = ""  # grub clears it as it boots it, whatever happens next (keep-next: it reads another environment block)
kernel = osu.kernel_of_entry(entries, ref) or "unknown"
if mode in ("ignore-env", "ignore-env-gaveup"):
    # a GRUB that reads no environment block at all: default="0", the first top-level menu entry that has a kernel (the newest installed)
    top = [e for e in entries if not e["parent"] and e["kernel"]]
    kernel = top[0]["kernel"] if top else "unknown"
with open(root + "/proc/sys/kernel/osrelease", "w") as f:
    f.write(kernel + "\n")
with open(root + "/proc/sys/kernel/random/boot_id", "w") as f:
    f.write(str(uuid.uuid4()) + "\n")
with open(root + "/proc/interrupts", "w") as f:
    f.write(" 135:  1200  0  ITS-MSI 4  Edge      enp49s0-8\n 167:  0  9  ITS-MSI 2  Edge      enp1s0-8\n")
if mode == "no-ping":
    st["ping"] = False
if mode == "no-nic":
    os.remove(root + "/sys/class/net/enp1s0/device")
with open(sp, "w") as f:
    json.dump(st, f)
if mode in ("booted-but-reported-failed", "keep-next-reported-failed", "ignore-env-gaveup"):
    sys.stderr.write("the reboot module gave up waiting, but the host rebooted (test)\n")
    sys.exit(1)
