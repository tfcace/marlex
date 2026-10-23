"""Verify the index with Google's flatc instead of marlex's own readers.

flatc decodes every segment and the reach overlay using only
schema/index.fbs; the decoded commits are then compared with git. A pass
means the files are valid FlatBuffers that generated code in any language
can read. Meant for small repositories (flatc's JSON is large).

usage: python3 -I bench/flatc_check.py FLATC REPO [INDEX_DIR]
"""
import glob, json, os, subprocess, sys, tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
SCHEMA = os.path.join(HERE, "..", "schema", "index.fbs")


def git(repo, *args):
    return subprocess.check_output(["git", "-C", repo, "-c", "core.quotePath=false", *args], text=True)


def main():
    flatc, repo = sys.argv[1], os.path.abspath(sys.argv[2])
    index = sys.argv[3] if len(sys.argv) > 3 else os.path.join(repo, ".git", "marlex")
    m = json.load(open(os.path.join(index, "manifest.json")))
    out = tempfile.mkdtemp(prefix="flatc-check-")

    def decode(name, root=None):
        cmd = [flatc, "--json", "--strict-json", "--raw-binary", "-o", out]
        if root:
            cmd += ["--root-type", root]
        subprocess.run(cmd + [SCHEMA, "--", os.path.join(index, name)], check=True)
        return json.load(open(os.path.join(out, name[:-3] + ".json")))

    segs = [decode(s["file"]) for s in m["segments"]]
    strings, hashes, bad = [], [], 0
    for s in segs:
        assert s.get("base_string", 0) == len(strings), "string ID ranges are not contiguous"
        strings += s.get("strings", [])
    for s in segs:
        n = len(s.get("times", []))
        if not n:
            continue
        assert s.get("base_commit", 0) == len(hashes), "commit index ranges are not contiguous"
        hashes += [bytes(s["hashes"][20 * i:20 * i + 20]).hex() for i in range(n)]
    for s in segs:
        base = s.get("base_commit", 0)
        po, co = s.get("parent_offsets", []), s.get("change_offsets", [])
        for i in range(len(s.get("times", []))):
            h = hashes[base + i]
            subj, parents = git(repo, "log", "-1", "--format=%s%x00%P", h).rstrip("\n").split("\x00")
            got_parents = [hashes[p] for p in s.get("parents", [])[po[i]:po[i + 1]]]
            got_paths = {strings[p] for p in s.get("change_paths", [])[co[i]:co[i + 1]]}
            # Against the first parent only, as the index records merges.
            first = parents.split()[:1] or ["--root"]
            want_paths = set(git(repo, "diff-tree", "--no-commit-id", "-r", "-M", "--name-only",
                                 *first, h).split("\n")) - {""}
            if subj != s["subjects"][i] or got_parents != parents.split() or got_paths != want_paths:
                bad += 1
                print(f"mismatch {h[:10]}")
    reach = decode(m["reach"], "marlex.fb.Reach") if m.get("reach") else {}
    ok = bad == 0 and len(hashes) == m["num_commits"] and len(strings) == m["num_strings"]
    print(f"flatc decoded {len(segs)} segments: {len(hashes)} commits ({bad} mismatches vs git), "
          f"{len(strings)} strings; reach: {len(reach.get('refs', []))} refs")
    print("PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)


main()
