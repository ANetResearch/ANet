#!/usr/bin/env python3
"""Set the escaped spaces that inline markup next to Chinese text needs.

    python3 docs/_tools/fix_po_markup.py

docutils recognises the start and the end of inline markup (``literal``,
:role:`x`, **strong**, *emphasis*) only next to white space or punctuation.
Chinese text has no spaces, so a translation needs a backslash and a space
between a Chinese character and the markup. This script adds them to every
translated message of the catalogs under docs/locale/zh_CN. It is idempotent:
run it after every `sphinx-intl update` and after editing a translation.
It needs the package Babel, which Sphinx installs.
"""
import glob
import pathlib
import re
import unicodedata as ud

from babel.messages.pofile import read_po, write_po

LOCALE = pathlib.Path(__file__).resolve().parent.parent / "locale" / "zh_CN" / "LC_MESSAGES"

SPAN = re.compile(
    r"(:[A-Za-z][\w:.+-]*:`[^`]+`)|(``(?:[^`]|`(?!`))+?``)|(`[^`]+`_{1,2})|(\*\*[^*]+?\*\*)"
    r"|((?<![*A-Za-z0-9_])\*[^*\s][^*]*?\*(?![*A-Za-z0-9_]))|(`[^`]+`)",
    re.S,
)


def ok_before(c):
    # docutils: white space, the ASCII characters ' " ( [ { < - / : and the
    # non-ASCII opening and delimiter punctuation. ASCII "," ";" "." "!" "?"
    # are not allowed before a start-string.
    if ord(c) < 128:
        return c.isspace() or c in "'\"([{<-/:"
    return ud.category(c) in ("Pi", "Ps", "Pd", "Po")


def ok_after(c):
    return c.isspace() or c in "-/:.,;!?\\)]}>'\"" or ud.category(c) in ("Pd", "Po", "Pe", "Pf")


def fix(s):
    out = []
    last = 0
    for m in SPAN.finditer(s):
        a, b = m.span()
        out.append(s[last:a])
        pre = "".join(out)
        t = m.group(0)
        if pre and not ok_before(pre[-1]):
            t = "\\ " + t
        if b < len(s) and not ok_after(s[b]):
            t = t + "\\ "
        out.append(t)
        last = b
    out.append(s[last:])
    return "".join(out)


def main():
    changed = 0
    for f in sorted(glob.glob(str(LOCALE / "**" / "*.po"), recursive=True)):
        with open(f, "rb") as fh:
            cat = read_po(fh)
        touched = False
        for m in cat:
            if m.id and isinstance(m.string, str):
                ns = fix(m.string)
                if ns != m.string:
                    m.string = ns
                    touched = True
                    changed += 1
        if touched:
            with open(f, "wb") as fh:
                write_po(fh, cat, width=76, include_previous=False)
    print("messages changed: %d" % changed)


if __name__ == "__main__":
    main()
