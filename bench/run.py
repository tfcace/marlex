"""Reproducible benchmark for the marlex index.

Measures, for one repository:
  build    time and size of a fresh index
  latency  git (with commit-graph + Bloom filters) vs marlex, as a CLI
           process and in-process (index opened once)
  exact    non-merge path history vs git log --all --full-history --no-merges
  shallow  depth-1 clone + pulled index answers the same as the full clone

usage:
  python3 -I bench/run.py run REPO --label NAME [--paths 50] [--out DIR]
  python3 -I bench/run.py compare A.json B.json

Results are written to DIR/<repo>-<label>.json so runs before and after a
format change can be compared.
"""
import argparse, json, os, random, shutil, statistics, subprocess, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
MX = os.path.join(HERE, "..", "bin", "mx")


def sh(cmd, cwd=None, check=True):
    r = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    if check and r.returncode != 0:
        sys.exit(f"{' '.join(cmd)}: {r.stderr.strip()}")
    return r.stdout


def best_ms(cmd, cwd=None, runs=5):
    b = float("inf")
    for _ in range(runs):
        t = time.perf_counter()
        subprocess.run(cmd, cwd=cwd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        b = min(b, time.perf_counter() - t)
    return b * 1000


def dir_size(d, prefix=""):
    return sum(os.path.getsize(os.path.join(d, f)) for f in os.listdir(d)
               if (f.endswith(".fb") or f == "manifest.json") and f.startswith(prefix))


def gp(index, *args, cwd):
    return [MX, "-C", cwd, "--index", index, *args]


def pick_paths(repo, index, n):
    hot = sh(gp(index, "hotspots", "--since", "100y", "-n", "10", cwd=repo)).splitlines()[1:]
    paths = [l.split()[-1] for l in hot]
    files = sh(["git", "ls-files"], cwd=repo).split()
    rnd = random.Random(7)
    for f in rnd.sample(files, min(len(files), n * 3)):
        if len(paths) >= n:
            break
        if f not in paths:
            paths.append(f)
    return paths


def summarize(xs):
    xs = sorted(xs)
    return {"median": statistics.median(xs), "p90": xs[int(0.9 * (len(xs) - 1))], "max": xs[-1]}


def run(args):
    repo = os.path.abspath(args.repo)
    name = os.path.basename(repo.rstrip("/"))
    work = tempfile.mkdtemp(prefix=f"mx-bench-{name}-")
    index = os.path.join(work, "index")
    res = {"repo": name, "label": args.label, "head": sh(["git", "rev-parse", "HEAD"], cwd=repo).strip(),
           "commits_all": int(sh(["git", "rev-list", "--count", "--all"], cwd=repo))}

    print(f"[{name}] git commit-graph with changed-path Bloom filters")
    t = time.perf_counter()
    sh(["git", "commit-graph", "write", "--reachable", "--changed-paths"], cwd=repo)
    res["git_commit_graph_s"] = time.perf_counter() - t

    print(f"[{name}] build")
    t = time.perf_counter()
    sh(gp(index, "build", "--no-compact", *(["--hunks"] if args.hunks else []), cwd=repo))
    res["build_s"] = time.perf_counter() - t
    res["core_build_s"] = res["build_s"]
    if args.hunks:
        # Hunks change the build, so time the core-only build on its own too:
        # comparisons across runs should be core against core.
        t = time.perf_counter()
        sh(gp(os.path.join(work, "core-index"), "build", "--no-compact", cwd=repo))
        res["core_build_s"] = time.perf_counter() - t
    manifest = json.load(open(os.path.join(index, "manifest.json")))
    res["index_commits"] = manifest["num_commits"]
    res["index_strings"] = manifest["num_strings"]
    res["index_bytes"] = dir_size(index)
    res["segment_bytes"] = sum(s["bytes"] for s in manifest["segments"])
    res["reach_bytes"] = manifest.get("reach_bytes", 0)
    res["blobs_bytes"] = sum(s.get("blob_bytes", 0) for s in manifest["segments"])
    res["hunks_bytes"] = sum(s.get("hunk_bytes", 0) for s in manifest["segments"])
    res["hunks"] = bool(args.hunks)
    res["core_bytes"] = res["index_bytes"] - res["blobs_bytes"] - res["hunks_bytes"]

    paths = pick_paths(repo, index, args.paths)
    res["paths"] = paths

    print(f"[{name}] latency over {len(paths)} paths")
    lat = {k: [] for k in ("git_log", "git_follow", "cli_log", "cli_follow")}
    for p in paths:
        lat["git_log"].append(best_ms(["git", "log", "--format=%H", "--", p], cwd=repo))
        lat["git_follow"].append(best_ms(["git", "log", "--follow", "--format=%H", "--", p], cwd=repo))
        lat["cli_log"].append(best_ms(gp(index, "log", p, cwd=repo), cwd=repo))
        lat["cli_follow"].append(best_ms(gp(index, "log", "--follow", p, cwd=repo), cwd=repo))
    res["process_floor_ms"] = best_ms(["/usr/bin/true"], runs=20)
    res["git_floor_ms"] = best_ms(["git", "rev-parse", "HEAD"], cwd=repo, runs=20)
    q = json.loads(subprocess.run(gp(index, "qbench", "--runs", "20", cwd=repo), input="\n".join(paths),
                                  capture_output=True, text=True, check=True).stdout)
    res["inproc_open_ms"] = q["open_us"] / 1000
    lat["inproc_log"] = [r["log_us"] / 1000 for r in q["rows"]]
    lat["inproc_follow"] = [r["follow_us"] / 1000 for r in q["rows"]]
    lat["inproc_cochange"] = [r["cochange_us"] / 1000 for r in q["rows"]]
    if args.hunks:
        lat["inproc_blame"] = [r["blame_us"] / 1000 for r in q["rows"] if r.get("blame_lines")]
        blame_paths = [r["path"] for r in q["rows"] if r.get("blame_lines")
                       and b"\0" not in subprocess.run(["git", "cat-file", "blob", "HEAD:" + r["path"]], cwd=repo,
                                                       capture_output=True).stdout]
        res["blame_paths"] = blame_paths
        print(f"[{name}] blame over {len(blame_paths)} text files")
        lat["git_blame"], lat["cli_blame"], bad = [], [], []
        for p in blame_paths:
            git_cmd = ["git", "-c", "diff.algorithm=myers", "blame", "--first-parent", "--porcelain", "HEAD", "--", p]
            lat["git_blame"].append(best_ms(git_cmd, cwd=repo, runs=3))
            lat["cli_blame"].append(best_ms(gp(index, "blame", p, cwd=repo), cwd=repo, runs=3))
            want = [l.split()[0] for l in sh(git_cmd, cwd=repo).splitlines()
                    if not l.startswith("\t") and len(l.split()) >= 3 and len(l.split()[0]) == 40]
            got = sh(gp(index, "blame", "--hashes", p, cwd=repo)).split()
            if got != want:
                bad.append({"path": p, "lines": len(want), "differ": sum(a != b for a, b in zip(got, want)) + abs(len(got) - len(want))})
        res["blame_exact"] = {"paths": len(blame_paths), "mismatches": bad,
                              "lines": sum(int(r["blame_lines"]) for r in q["rows"] if r["path"] in blame_paths)}
    res["latency_ms"] = {k: summarize(v) for k, v in lat.items()}

    print(f"[{name}] exactness")
    mism = []
    for p in paths:
        want = set(sh(["git", "log", "--all", "--full-history", "--no-merges", "--format=%H", "--", p], cwd=repo).split())
        rows = json.loads(sh(gp(index, "log", p, "--rev", "all", "--json", cwd=repo)) or "null") or []
        got = {r["hash"] for r in rows if not r["merge"]}
        if got != want:
            mism.append({"path": p, "index_only": len(got - want), "git_only": len(want - got)})
    res["exact"] = {"paths": len(paths), "mismatches": mism}

    print(f"[{name}] shallow clone")
    bucket = os.path.join(work, "bucket")
    sh(gp(index, "publish", bucket, cwd=repo))
    shallow = os.path.join(work, "shallow")
    t = time.perf_counter()
    sh(["git", "clone", "-q", "--depth", "1", "--no-local", "file://" + repo, shallow])
    res["shallow_clone_s"] = time.perf_counter() - t
    res["shallow_git_bytes"] = int(sh(["du", "-sk", os.path.join(shallow, ".git")]).split()[0]) * 1024
    res["full_git_bytes"] = int(sh(["du", "-sk", os.path.join(repo, ".git")]).split()[0]) * 1024
    t = time.perf_counter()
    sh([MX, "-C", shallow, "pull", *(["--hunks"] if args.hunks else []), bucket])
    res["pull_s"] = time.perf_counter() - t
    res["pulled_bytes"] = dir_size(os.path.join(shallow, ".git", "marlex"))
    pulled = set(os.listdir(os.path.join(shallow, ".git", "marlex")))
    def transferred(names):
        return sum(os.path.getsize(os.path.join(bucket, f)) for f in os.listdir(bucket)
                   if f == "manifest.json" or f.removesuffix(".zst") in names)
    res["transferred_bytes"] = transferred(pulled)
    res["core_transferred_bytes"] = res["transferred_bytes"]
    if args.hunks:
        core_pull = os.path.join(work, "core-pull")
        sh([MX, "-C", shallow, "--index", core_pull, "pull", bucket])
        res["core_transferred_bytes"] = transferred(set(os.listdir(core_pull)))
        res["hunks_transferred_bytes"] = transferred({f for f in pulled if f.startswith("hunks-")}) - transferred(set())
    same, git_counts = 0, []
    for p in paths:
        full = [r["hash"] for r in json.loads(sh(gp(index, "log", "--follow", p, "--json", cwd=repo)) or "null") or []]
        sh_rows = json.loads(sh([MX, "-C", shallow, "log", "--follow", p, "--json"]) or "null") or []
        same += [r["hash"] for r in sh_rows] == full
        git_counts.append(len(sh(["git", "log", "--follow", "--format=%H", "--", p], cwd=shallow, check=False).split()))
    if args.hunks:
        bsame = sum(sh([MX, "-C", shallow, "blame", "--hashes", p]) == sh(gp(index, "blame", "--hashes", p, cwd=repo))
                    for p in res["blame_paths"])
        res["shallow_blame_identical"] = bsame
    res["shallow"] = {"paths": len(paths), "identical_follow": same,
                      "git_follow_commits_max": max(git_counts), "git_follow_commits_mean": statistics.mean(git_counts)}

    shutil.rmtree(work)
    os.makedirs(args.out, exist_ok=True)
    out = os.path.join(args.out, f"{name}-{args.label}.json")
    json.dump(res, open(out, "w"), indent=2)
    report(res)
    print(f"\nwrote {out}")


def mb(n):
    return f"{n / 2**20:.1f} MB"


def report(r):
    L = r["latency_ms"]
    print(f"\n## {r['repo']} ({r['label']}): {r['index_commits']} commits indexed\n")
    build = f"build {r.get('core_build_s', r['build_s']):.2f}s" + (f" ({r['build_s']:.2f}s with hunks)" if r.get("hunks") else "")
    print(f"{build}, index {mb(r['index_bytes'])}: core {mb(r.get('core_bytes', r['index_bytes']))} "
          f"(segments {mb(r['segment_bytes'])}, reach {mb(r['reach_bytes'])}), optional blobs {mb(r.get('blobs_bytes', 0))}, "
          f"optional hunks {mb(r.get('hunks_bytes', 0))}; "
          f"git commit-graph {r['git_commit_graph_s']:.2f}s")
    print(f"\n| query (median / p90 over {r['exact']['paths']} paths) | git + Bloom | marlex CLI | marlex in-process |")
    print("|---|---|---|---|")
    print(f"| log -- path | {L['git_log']['median']:.1f} / {L['git_log']['p90']:.1f} ms | {L['cli_log']['median']:.1f} / {L['cli_log']['p90']:.1f} ms "
          f"| {1000*L['inproc_log']['median']:.1f} / {1000*L['inproc_log']['p90']:.1f} µs |")
    print(f"| log --follow | {L['git_follow']['median']:.1f} / {L['git_follow']['p90']:.1f} ms | {L['cli_follow']['median']:.1f} / {L['cli_follow']['p90']:.1f} ms "
          f"| {1000*L['inproc_follow']['median']:.1f} / {1000*L['inproc_follow']['p90']:.1f} µs |")
    print(f"| cochange | n/a | n/a | {L['inproc_cochange']['median']:.2f} / {L['inproc_cochange']['p90']:.2f} ms |")
    if r.get("hunks"):
        print(f"| blame (whole file) | {L['git_blame']['median']:.1f} / {L['git_blame']['p90']:.1f} ms | {L['cli_blame']['median']:.1f} / {L['cli_blame']['p90']:.1f} ms "
              f"| {1000*L['inproc_blame']['median']:.1f} / {1000*L['inproc_blame']['p90']:.1f} µs |")
    print(f"\nfloors: process {r['process_floor_ms']:.1f} ms, git startup {r['git_floor_ms']:.1f} ms, index open {r['inproc_open_ms']:.2f} ms")
    e = r["exact"]
    print(f"exactness: {e['paths'] - len(e['mismatches'])}/{e['paths']} paths match git" +
          (f"; mismatches {e['mismatches']}" if e["mismatches"] else ""))
    if r.get("blame_exact"):
        b = r["blame_exact"]
        print(f"blame: {b['paths'] - len(b['mismatches'])}/{b['paths']} files ({b['lines']} lines) identical to git blame --first-parent; "
              f"shallow clone blame identical for {r.get('shallow_blame_identical')}/{b['paths']}" +
              (f"; mismatches {b['mismatches']}" if b["mismatches"] else ""))
    s = r["shallow"]
    print(f"shallow: depth-1 .git {mb(r['shallow_git_bytes'])} + pulled index {mb(r.get('pulled_bytes', r['index_bytes']))} "
          f"({mb(r.get('transferred_bytes', r.get('pulled_bytes', r['index_bytes'])))} transferred"
          + (f", core alone {mb(r['core_transferred_bytes'])}" if r.get("hunks") and "core_transferred_bytes" in r else "")
          + f") vs full .git {mb(r['full_git_bytes'])}; "
          f"pull {r['pull_s']:.2f}s; {s['identical_follow']}/{s['paths']} follow histories identical to the full clone; "
          f"git log --follow in the shallow clone finds at most {s['git_follow_commits_max']} commit(s)")


def compare(args):
    """Compare two runs core against core; hunk costs get their own rows.

    Runs from before the core/hunks split (v1 baselines) were core-only and
    pulled uncompressed, so their whole index is their core.
    """
    a, b = json.load(open(args.a)), json.load(open(args.b))
    MB = 2**20

    def core_transfer(r):
        if "core_transferred_bytes" in r:
            return r["core_transferred_bytes"] / MB
        if r.get("hunks"):
            return None  # a run that pulled with hunks and did not record the core alone
        return r.get("transferred_bytes", r.get("pulled_bytes", r["index_bytes"])) / MB

    def core_build(r):
        if "core_build_s" in r:
            return r["core_build_s"]
        return None if r.get("hunks") else r["build_s"]

    def latency(key, scale=1):
        return lambda r: scale * r["latency_ms"][key]["median"] if key in r["latency_ms"] else None

    rows = [
        ("core MB on disk", lambda r: r.get("core_bytes", r["index_bytes"]) / MB),
        ("core MB transferred by pull", core_transfer),
        ("core build s", core_build),
        ("hunks MB on disk", lambda r: r["hunks_bytes"] / MB if r.get("hunks") else None),
        ("hunks MB transferred", lambda r: r["hunks_transferred_bytes"] / MB if "hunks_transferred_bytes" in r else None),
        ("build with hunks s", lambda r: r["build_s"] if r.get("hunks") else None),
        ("CLI log median ms", latency("cli_log")),
        ("CLI follow median ms", latency("cli_follow")),
        ("CLI blame median ms", latency("cli_blame")),
        ("in-process log median µs", latency("inproc_log", 1000)),
        ("in-process follow median µs", latency("inproc_follow", 1000)),
        ("index open ms", lambda r: r["inproc_open_ms"]),
        ("history mismatches vs git", lambda r: len(r["exact"]["mismatches"])),
        ("blame mismatches vs git", lambda r: len(r["blame_exact"]["mismatches"]) if "blame_exact" in r else None),
        ("shallow follow identical", lambda r: r["shallow"]["identical_follow"]),
    ]
    fmt = lambda v: "n/a" if v is None else f"{v:.2f}"
    print(f"| metric | {a['label']} | {b['label']} | change |\n|---|---|---|---|")
    for name, f in rows:
        x, y = f(a), f(b)
        ch = f"{(y - x) / x:+.0%}" if x and y is not None else "n/a"
        print(f"| {name} | {fmt(x)} | {fmt(y)} | {ch} |")


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("repo")
    r.add_argument("--label", required=True)
    r.add_argument("--paths", type=int, default=50)
    r.add_argument("--out", default=os.path.join(HERE, "results"))
    r.add_argument("--hunks", action="store_true", help="build with hunks and benchmark blame")
    c = sub.add_parser("compare")
    c.add_argument("a")
    c.add_argument("b")
    a = ap.parse_args()
    run(a) if a.cmd == "run" else compare(a)
