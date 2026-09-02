#!/usr/bin/env python3
"""Rewrite legacy CLI paths in maintained prose to canonical spellings."""
import re, os, subprocess, sys, collections

WD = sys.argv[1] if len(sys.argv) > 1 else "warden"
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

MAINTAINED_DIRS = [
    "site/src/content/docs/start",
    "site/src/content/docs/guides",
    "site/src/content/docs/concepts",
    "site/src/content/docs/reference",
    "site/src/content/docs/multi-agent",
    "skills",
    "docs/agent-backends",
]
MAINTAINED_FILES = ["README.md", "docs/USAGE.md", "docs/FEATURES.md"]
SKIP = {"site/src/content/docs/reference/cli.md", "docs/specs/"}


def load_mapping():
    out = subprocess.run([WD, "help", "--all"], capture_output=True, text=True, check=True).stdout
    appendix = out.split("Compatibility aliases:", 1)[1]
    mapping = {}
    for line in appendix.splitlines():
        line = line.strip()
        m = re.match(r"^warden (.+?) -> warden (.+?) \(compatibility\)$", line)
        if not m:
            continue
        legacy, canonical = m.group(1), m.group(2)
        if legacy == canonical or len(legacy) < 3:
            continue
        mapping[legacy] = canonical
    return mapping


def targets():
    found = []
    for d in MAINTAINED_DIRS:
        path = os.path.join(ROOT, d)
        if not os.path.isdir(path):
            continue
        for dp, dn, fn in os.walk(path):
            dn[:] = [x for x in dn if x not in ("node_modules", ".git")]
            for f in fn:
                if f.endswith((".md", ".mdx")):
                    found.append(os.path.join(dp, f))
    for f in MAINTAINED_FILES:
        p = os.path.join(ROOT, f)
        if os.path.isfile(p):
            found.append(p)
    return [f for f in found if not any(s in f for s in SKIP)]


def main():
    mapping = load_mapping()
    ordered = sorted(mapping.items(), key=lambda kv: -len(kv[0]))
    patterns = [
        (re.compile(r"\b(wd|warden)(\s+)" + re.escape(legacy) + r"\b"), canonical)
        for legacy, canonical in ordered
    ]
    counts = collections.Counter()
    for path in targets():
        src = open(path).read()
        out = src
        for pat, canonical in patterns:
            out, n = pat.subn(lambda m, c=canonical: m.group(1) + m.group(2) + c, out)
            if n:
                counts[path] += n
        if out != src:
            open(path, "w").write(out)
    print(f"rewrote {sum(counts.values())} legacy paths across {len(counts)} files")


if __name__ == "__main__":
    main()
