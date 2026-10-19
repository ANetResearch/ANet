#!/usr/bin/env python3
"""Generate the anet plugins of every ecosystem from one set of sources.

    python3 deploy/plugins/build.py --out DIR [--target KEY ...] [--force]
    python3 deploy/plugins/build.py --check [DIR] [--target KEY ...]
    python3 deploy/plugins/build.py --site DIR [--site-base URL] [--target KEY ...]
    python3 deploy/plugins/build.py --list

--out DIR       write DIR/<key>/ (the plugin of each target) and the files that a
                target places at the root (catalogues, root manifests), plus
                README.md and LICENSE of the root when every target is built.
                The committed tree is deploy/plugins/dist:
                    python3 deploy/plugins/build.py --out deploy/plugins/dist --force --prune
--force         replace DIR/<key>/ and root files that already exist.
--prune         with --out and a full build: delete the files of DIR that the
                generator does not produce (for the committed dist/ only).
--check [DIR]   compare a fresh generation with DIR (default deploy/plugins/dist)
                and fail on any difference; nothing is written.
--site DIR      write the catalogues and archives for HTTPS hosting, to be copied
                to the web root of https://agentnetwork.org.cn (a target chooses its
                own directory, by default plugins/<key>/).
--site-base URL the public root of the site (default https://agentnetwork.org.cn).
--target KEY    build only these targets (repeatable). A module whose name starts
                with an underscore is built only when named here.

Nothing is published by this script.
"""

import argparse
import sys
from pathlib import Path

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))

import generator as g  # noqa: E402


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", type=Path)
    ap.add_argument("--force", action="store_true")
    ap.add_argument("--prune", action="store_true")
    ap.add_argument("--check", nargs="?", const=str(g.DIST), default=None, metavar="DIR")
    ap.add_argument("--site", type=Path)
    ap.add_argument("--site-base", default=g.SITE_ROOT_URL)
    ap.add_argument("--target", action="append", default=[])
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args(argv)
    if not (args.out or args.check is not None or args.site or args.list):
        ap.error("nothing to do: pass --out, --check, --site or --list")
    if args.prune and not args.out:
        ap.error("--prune needs --out")
    if args.prune and args.target:
        ap.error("--prune needs a full build: do not pass --target")

    try:
        if args.list:
            for key, mod in g.discover(args.target or None).items():
                print(f"{key}\t{mod.TITLE}\t{mod.HELP}")
            return 0
        res = g.build_all(args.target or None)
        tree = res.tree()
        keys = list(res.contexts)
        if args.check is not None:
            problems = g.compare_tree(tree, args.check, keys if args.target else None)
            if problems:
                print(f"build.py: {args.check} differs from a fresh generation:", file=sys.stderr)
                for line in problems:
                    print(f"  {line}", file=sys.stderr)
                print("regenerate it: python3 deploy/plugins/build.py --out deploy/plugins/dist --force --prune", file=sys.stderr)
                return 1
            print(f"{args.check}: up to date ({len(tree)} files, {len(keys)} targets)")
        if args.out:
            g.write_tree(tree, args.out, owned_dirs=keys, force=args.force)
            print(f"wrote {len(tree)} files for {', '.join(keys)} to {args.out}")
            if args.prune:
                for rel in g.prune_tree(tree, args.out):
                    print(f"pruned {rel}")
        if args.site:
            site_tree = g.build_site(res, args.site_base)
            g.write_tree(site_tree, args.site, force=args.force)
            for rel, f in sorted(site_tree.items()):
                print(f"site: {rel} ({len(f.data)} bytes)")
    except (g.BuildError, ValueError) as e:
        print(f"build.py: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
