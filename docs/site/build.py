#!/usr/bin/env python3
"""build.py — render docs/*-zh.md into self-contained HTML for publication.

Standard library only. The Markdown subset it understands is exactly the
subset the source documents use: ATX headings, paragraphs, pipe tables,
fenced code, blockquotes, flat lists, horizontal rules, and the inline
forms code / bold / italic / strike / link.

    python3 docs/site/build.py docs/DESIGN-zh.md docs/site/design.html [eyebrow] [--links=github]

--links=github rewrites sibling *-zh.md links to the repository instead of to
a neighbouring .html. A page published on its own — as an artifact, or pasted
somewhere — has no neighbours, and a relative link that 404s is worse than a
link to the source.
"""
import html
import re
import sys
from pathlib import Path

# ---------------------------------------------------------------- inline

_INLINE_CODE = re.compile(r"`([^`]+)`")
_BOLD = re.compile(r"\*\*(.+?)\*\*")
_ITALIC = re.compile(r"(?<![\w*])\*([^*\n]+?)\*(?![\w*])")
_STRIKE = re.compile(r"~~(.+?)~~")
_LINK = re.compile(r"\[([^\]]+)\]\(([^)\s]+)\)")


def inline(text: str) -> str:
    # Protect code spans before any other substitution touches their bytes.
    spans = []

    def stash(m):
        spans.append(html.escape(m.group(1)))
        return f"\x00{len(spans) - 1}\x00"

    text = _INLINE_CODE.sub(stash, text)
    text = html.escape(text, quote=False)
    text = _LINK.sub(lambda m: f'<a href="{_href(m.group(2))}">{m.group(1)}</a>', text)
    text = _BOLD.sub(r"<strong>\1</strong>", text)
    text = _STRIKE.sub(r"<s>\1</s>", text)
    text = _ITALIC.sub(r"<em>\1</em>", text)
    text = re.sub(r"\x00(\d+)\x00", lambda m: f"<code>{spans[int(m.group(1))]}</code>", text)
    return text


REPO_DOCS = "https://github.com/ANetResearch/ANet/blob/main/docs/"
LINK_MODE = "site"


def _href(target: str) -> str:
    if not target.endswith(".md"):
        return target
    if LINK_MODE == "github":
        return REPO_DOCS + target
    # Sibling docs are published beside each other as .html.
    stem = target[:-len("-zh.md")] if target.endswith("-zh.md") else target[:-3]
    return stem.lower() + ".html"


# ---------------------------------------------------------------- blocks

def slug(text: str, seen: set) -> str:
    base = re.sub(r"[^\w\u4e00-\u9fff-]+", "-", re.sub(r"<[^>]+>", "", text)).strip("-").lower() or "s"
    s, n = base, 2
    while s in seen:
        s, n = f"{base}-{n}", n + 1
    seen.add(s)
    return s


def render(md: str):
    lines = md.splitlines()
    out, toc, seen = [], [], set()
    title = ""
    i, n = 0, len(lines)

    def para_flush(buf):
        if buf:
            out.append(f"<p>{inline(' '.join(s.strip() for s in buf))}</p>")
            buf.clear()

    buf = []
    while i < n:
        line = lines[i]
        s = line.strip()

        # fenced code
        if s.startswith("```"):
            para_flush(buf)
            lang = s[3:].strip()
            i += 1
            code = []
            while i < n and not lines[i].strip().startswith("```"):
                code.append(lines[i])
                i += 1
            i += 1
            cls = f' class="lang-{html.escape(lang)}"' if lang else ""
            out.append(f'<div class="code"><button class="copy" type="button">复制</button><pre{cls}>{html.escape(chr(10).join(code))}</pre></div>')
            continue

        # heading
        m = re.match(r"^(#{1,4})\s+(.*)$", s)
        if m:
            para_flush(buf)
            level = len(m.group(1))
            text = inline(m.group(2))
            if level == 1 and not title:
                title = re.sub(r"<[^>]+>", "", text)
                out.append(f"<h1>{text}</h1>")
            else:
                sid = slug(m.group(2), seen)
                out.append(f'<h{level} id="{sid}">{text}</h{level}>')
                if level in (2, 3):
                    toc.append((level, sid, re.sub(r"<[^>]+>", "", text)))
            i += 1
            continue

        # rule
        if re.match(r"^-{3,}$", s):
            para_flush(buf)
            out.append("<hr>")
            i += 1
            continue

        # table
        if s.startswith("|") and i + 1 < n and re.match(r"^\|?\s*:?-{2,}", lines[i + 1].strip()):
            para_flush(buf)
            header = [c.strip() for c in s.strip("|").split("|")]
            i += 2
            rows = []
            while i < n and lines[i].strip().startswith("|"):
                rows.append([c.strip() for c in lines[i].strip().strip("|").split("|")])
                i += 1
            # A header of empty cells is a key/value block, not a table with
            # column names: render it without a header row.
            bare = all(h == "" for h in header)
            t = ['<div class="tw"><table>']
            if not bare:
                t.append("<thead><tr>" + "".join(f"<th>{inline(h)}</th>" for h in header) + "</tr></thead>")
            t.append("<tbody>")
            for r in rows:
                t.append("<tr>" + "".join(f"<td>{inline(c)}</td>" for c in r) + "</tr>")
            t.append("</tbody></table></div>")
            out.append("".join(t))
            continue

        # blockquote
        if s.startswith(">"):
            para_flush(buf)
            q = []
            while i < n and lines[i].strip().startswith(">"):
                q.append(lines[i].strip()[1:].strip())
                i += 1
            out.append(f'<blockquote>{inline(" ".join(x for x in q if x))}</blockquote>')
            continue

        # list
        if re.match(r"^(-|\d+\.)\s+", s):
            para_flush(buf)
            ordered = bool(re.match(r"^\d+\.", s))
            items = []
            while i < n and re.match(r"^(-|\d+\.)\s+", lines[i].strip()):
                item = re.sub(r"^(-|\d+\.)\s+", "", lines[i].strip())
                i += 1
                # continuation lines indented under the item
                while i < n and lines[i].startswith("  ") and lines[i].strip() and not re.match(r"^(-|\d+\.)\s+", lines[i].strip()):
                    item += " " + lines[i].strip()
                    i += 1
                items.append(f"<li>{inline(item)}</li>")
            tag = "ol" if ordered else "ul"
            out.append(f"<{tag}>{''.join(items)}</{tag}>")
            continue

        # blank
        if not s:
            para_flush(buf)
            i += 1
            continue

        buf.append(line)
        i += 1
    para_flush(buf)
    return title, "\n".join(out), toc


# ---------------------------------------------------------------- page

STYLE = r"""
:root{
  --ground:#FAFAF7;--surface:#FFFFFF;--sunken:#F1F2EE;--ink:#16191A;--ink-2:#3D4644;--ink-3:#6B7573;
  --rule:#DFE2DC;--rule-2:#C9CEC6;--accent:#0F6E5C;--accent-soft:#E4F0EC;--deny:#A8452C;--deny-soft:#F6E7E1;
  --code-ground:#14201D;--code-ink:#DCE7E2;--code-dim:#7E948D;--code-accent:#7FD3B8;
  --f-sans:"IBM Plex Sans","Noto Sans SC","PingFang SC","Hiragino Sans GB","Microsoft YaHei",system-ui,sans-serif;
  --f-mono:"IBM Plex Mono","SFMono-Regular",Menlo,Consolas,monospace;
}
@media (prefers-color-scheme: dark){:root:not([data-theme="light"]){
  --ground:#101413;--surface:#171C1B;--sunken:#1D2422;--ink:#E8EDEA;--ink-2:#BDC7C3;--ink-3:#8B9793;
  --rule:#2A3230;--rule-2:#3A4441;--accent:#4FBFA3;--accent-soft:#17302A;--deny:#E08D74;--deny-soft:#2E1E19;
  --code-ground:#0B1210;--code-ink:#DCE7E2;--code-dim:#6E827C;--code-accent:#7FD3B8;}}
:root[data-theme="dark"]{
  --ground:#101413;--surface:#171C1B;--sunken:#1D2422;--ink:#E8EDEA;--ink-2:#BDC7C3;--ink-3:#8B9793;
  --rule:#2A3230;--rule-2:#3A4441;--accent:#4FBFA3;--accent-soft:#17302A;--deny:#E08D74;--deny-soft:#2E1E19;
  --code-ground:#0B1210;--code-ink:#DCE7E2;--code-dim:#6E827C;--code-accent:#7FD3B8;}
*{box-sizing:border-box}
body{margin:0;background:var(--ground);color:var(--ink);font-family:var(--f-sans);font-size:16px;line-height:1.72;-webkit-font-smoothing:antialiased}
.page{max-width:86rem;margin:0 auto;padding:0 1.5rem 6rem;display:grid;grid-template-columns:16rem minmax(0,1fr);gap:3rem}
@media (max-width:900px){.page{grid-template-columns:1fr;gap:0}}
nav.toc{position:sticky;top:1.25rem;align-self:start;max-height:calc(100vh - 2.5rem);overflow-y:auto;padding-top:3.6rem;font-size:.82rem}
@media (max-width:900px){nav.toc{position:static;max-height:none;padding-top:1.5rem;border-bottom:1px solid var(--rule);margin-bottom:1rem}}
nav.toc .eyebrow{font-family:var(--f-mono);font-size:.66rem;letter-spacing:.14em;text-transform:uppercase;color:var(--ink-3);margin:0 0 .6rem}
nav.toc a{display:block;color:var(--ink-2);text-decoration:none;padding:.18rem 0 .18rem .75rem;border-left:2px solid transparent;line-height:1.45}
nav.toc a.l3{padding-left:1.6rem;font-size:.78rem;color:var(--ink-3)}
nav.toc a:hover,nav.toc a:focus-visible{color:var(--accent);border-left-color:var(--accent)}
main{min-width:0;max-width:72ch}
.mast{padding:3.5rem 0 1.25rem;border-bottom:1px solid var(--rule);margin-bottom:2rem}
.mast .eyebrow{font-family:var(--f-mono);font-size:.72rem;letter-spacing:.14em;text-transform:uppercase;color:var(--ink-3);margin:0 0 .9rem}
h1{font-size:clamp(1.8rem,1.2rem + 2vw,2.6rem);line-height:1.15;font-weight:700;letter-spacing:-.02em;margin:0;text-wrap:balance}
h2{font-size:1.45rem;font-weight:600;letter-spacing:-.014em;margin:3rem 0 .9rem;padding-top:.5rem;text-wrap:balance}
h2::before{content:"";display:block;width:2.25rem;height:2px;background:var(--accent);margin-bottom:.9rem}
h3{font-size:1.08rem;font-weight:600;margin:2rem 0 .6rem}
h4{font-size:.95rem;font-weight:600;margin:1.5rem 0 .4rem;color:var(--ink-2)}
p{margin:0 0 1rem}
a{color:var(--accent)}
hr{border:none;border-top:1px solid var(--rule);margin:2.5rem 0}
blockquote{border-left:2px solid var(--accent);background:var(--accent-soft);padding:.85rem 1rem;margin:0 0 1.25rem;color:var(--ink-2);border-radius:0 4px 4px 0;font-size:.95rem}
blockquote strong{color:var(--ink)}
ul,ol{margin:0 0 1rem;padding-left:1.25rem}li{margin-bottom:.35rem}
code{font-family:var(--f-mono);font-size:.86em;background:var(--sunken);padding:.1em .34em;border-radius:3px;word-break:break-word}
.code{position:relative;background:var(--code-ground);border-radius:6px;margin:0 0 1.25rem;overflow:hidden}
.code pre{margin:0;padding:1rem 1.1rem;overflow-x:auto;font-family:var(--f-mono);font-size:.82rem;line-height:1.7;color:var(--code-ink);tab-size:2}
.copy{position:absolute;top:.5rem;right:.5rem;font-family:var(--f-mono);font-size:.66rem;letter-spacing:.06em;text-transform:uppercase;color:var(--code-dim);background:transparent;border:1px solid rgba(255,255,255,.14);border-radius:4px;padding:.2rem .5rem;cursor:pointer}
.copy:hover,.copy:focus-visible{color:var(--code-accent);border-color:rgba(127,211,184,.45)}
.tw{overflow-x:auto;margin:0 0 1.25rem;border:1px solid var(--rule);border-radius:6px}
table{border-collapse:collapse;width:100%;font-size:.87rem}
th,td{text-align:left;padding:.58rem .8rem;border-bottom:1px solid var(--rule);vertical-align:top;line-height:1.55}
thead th{font-family:var(--f-mono);font-size:.66rem;letter-spacing:.1em;text-transform:uppercase;color:var(--ink-3);font-weight:500;background:var(--sunken);white-space:nowrap}
tbody tr:last-child td{border-bottom:none}
td code{background:transparent;padding:0;font-size:.82em}
.mast + .tw table{font-size:.85rem}
footer{margin-top:4rem;padding-top:1.25rem;border-top:1px solid var(--rule);font-size:.8rem;color:var(--ink-3)}
@media (prefers-reduced-motion:reduce){*{transition:none!important}}
"""

SCRIPT = """
document.querySelectorAll(".copy").forEach(function(b){b.addEventListener("click",function(){
  var t=b.parentElement.querySelector("pre").innerText.replace(/[ \\t]+$/gm,"");
  navigator.clipboard.writeText(t).then(function(){var o=b.textContent;b.textContent="已复制";setTimeout(function(){b.textContent=o},1400)}).catch(function(){});
})});
"""


def page(title: str, body: str, toc, eyebrow: str) -> str:
    nav = "".join(
        f'<a class="l{lvl}" href="#{sid}">{html.escape(text)}</a>' for lvl, sid, text in toc
    )
    return f"""<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{html.escape(title)}</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&family=IBM+Plex+Sans:wght@400;500;600;700&display=swap">
<style>{STYLE}</style>
</head>
<body>
<div class="page">
<nav class="toc"><p class="eyebrow">目录</p>{nav}</nav>
<main>
<header class="mast"><p class="eyebrow">{html.escape(eyebrow)}</p>{body.split(chr(10), 1)[0]}</header>
{body.split(chr(10), 1)[1] if chr(10) in body else ''}
<footer>ANet · agentnetwork.org.cn · 源码 github.com/ANetResearch</footer>
</main>
</div>
<script>{SCRIPT}</script>
</body>
</html>
"""


def main():
    global LINK_MODE
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    if "--links=github" in sys.argv:
        LINK_MODE = "github"
    if len(args) < 2:
        sys.exit(__doc__)
    src, dst = Path(args[0]), Path(args[1])
    eyebrow = args[2] if len(args) > 2 else "ANet"
    title, body, toc = render(src.read_text(encoding="utf-8"))
    dst.parent.mkdir(parents=True, exist_ok=True)
    dst.write_text(page(title, body, toc, eyebrow), encoding="utf-8")
    print(f"{dst}  {dst.stat().st_size // 1024} K  h2/h3={len(toc)}")


if __name__ == "__main__":
    main()
