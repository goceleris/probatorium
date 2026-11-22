#!/usr/bin/env python3
"""Adapt a HttpArena checkout to a small host (msa2-server: 16 CPUs) WITHOUT touching the host.

  host_adapt.py <HttpArena-clone> [--lscpu FILE]      FILE = output of `lscpu -p=CPU,CORE` (tests); default: run it here

Writes, inside the clone only:
  scripts/lib/profiles.sh      the hard-coded server cpuset "0-31,64-95" replaced (original kept as profiles.sh.orig)
  scripts/benchmark.sh         the two `docker volume prune` / `docker image prune` lines at startup and in cleanup_all
                               commented out (they would prune dangling images/volumes of OTHER users of a shared host)
  preview.env                  the environment benchmark.sh reads (source it)

Layout (HttpArena gives the server 64 of the 128 logical CPUs, the generator the other 64; DB profiles 32/32/64):
  plain profiles   server = first half of the cores (SMT siblings included), generator = second half
  DB profiles      server = first quarter, generator = second quarter, postgres = the remaining half
"""
import re, subprocess, sys, pathlib, collections

def cpulist(cpus):
    cpus = sorted(cpus); out = []; i = 0
    while i < len(cpus):
        j = i
        while j + 1 < len(cpus) and cpus[j + 1] == cpus[j] + 1: j += 1
        out.append(str(cpus[i]) if i == j else f"{cpus[i]}-{cpus[j]}"); i = j + 1
    return ",".join(out)

def main():
    ha = pathlib.Path(sys.argv[1]).resolve()
    txt = (pathlib.Path(sys.argv[sys.argv.index("--lscpu") + 1]).read_text() if "--lscpu" in sys.argv
           else subprocess.run(["lscpu", "-p=CPU,CORE"], capture_output=True, text=True, check=True).stdout)
    cores = collections.OrderedDict()
    for line in txt.splitlines():
        if line.startswith("#") or not line.strip(): continue
        cpu, core = (int(x) for x in line.split(",")[:2])
        cores.setdefault(core, []).append(cpu)
    groups = [sorted(v) for _, v in sorted(cores.items(), key=lambda kv: min(kv[1]))]
    n = len(groups)
    if n < 4: sys.exit(f"only {n} cores: too small to split server/generator/postgres")
    half, quarter = n // 2, n // 4
    flat = lambda gs: [c for g in gs for c in g]
    server, gen = flat(groups[:half]), flat(groups[half:])
    db_server, db_gen, db_pg = flat(groups[:quarter]), flat(groups[quarter:2 * quarter]), flat(groups[2 * quarter:])
    env = {
        "SKIP_TUNE": "true", "LOADGEN_DOCKER": "true",
        "GCANNON_CPUS": cpulist(gen), "THREADS": len(gen), "H2THREADS": len(gen), "ASYNC_THREADS": len(gen),
        "DB_SERVER_CPUS": cpulist(db_server), "DB_LOADGEN_CPUS": cpulist(db_gen), "DB_POSTGRES_CPUS": cpulist(db_pg),
        "DB_THREADS": len(db_gen),
    }
    pl = ha / "scripts/lib/profiles.sh"
    orig = ha / "scripts/lib/profiles.sh.orig"
    if not orig.exists(): orig.write_text(pl.read_text())
    s = orig.read_text()
    assert "0-31,64-95" in s, "profiles.sh has no hard-coded server cpuset (HttpArena changed?)"
    pl.write_text(s.replace("0-31,64-95", cpulist(server)))
    bm = ha / "scripts/benchmark.sh"
    b = bm.read_text()
    b = re.sub(r"^(\s*)(docker (?:volume|image) prune\s+-f[^\n]*)$", r"\1# preview: not on a shared host: \2", b, flags=re.M)
    bm.write_text(b)
    (ha / "preview.env").write_text("".join(f"export {k}={v}\n" for k, v in env.items()))
    print(f"cores={n} logical={sum(len(g) for g in groups)}  server={cpulist(server)}  generator={cpulist(gen)}")
    print(f"DB: server={cpulist(db_server)} generator={cpulist(db_gen)} postgres={cpulist(db_pg)}")
    print(f"wrote {pl}, {bm} (prunes off), {ha/'preview.env'}")

main()
