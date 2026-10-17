# Configuration file for the ANet documentation (Sphinx).
import os

project = "ANet"
author = "Agent Network"
copyright = "Agent Network"
release = "0.3.1"
version = "0.3"

# English is the source language; translations are selected with SPHINX_LANGUAGE
# (or the Read the Docs READTHEDOCS_LANGUAGE variable), for example "zh_CN".
language = os.environ.get("SPHINX_LANGUAGE") or os.environ.get("READTHEDOCS_LANGUAGE") or "en"
locale_dirs = ["locale/"]
gettext_compact = False

extensions = [
    "sphinx_copybutton",
]

root_doc = "index"
source_suffix = {".rst": "restructuredtext"}
templates_path = []
exclude_patterns = [
    "_build",
    "notes",
    "a2a",
    "*.md",
    "**/*.md",
    "Thumbs.db",
    ".DS_Store",
]
pygments_style = "default"
nitpicky = False

html_theme = "sphinx_rtd_theme"
html_logo = "_static/anet-logo.svg"
html_favicon = "_static/favicon.ico"
html_static_path = ["_static"]
html_css_files = ["custom.css"]
html_title = "ANet documentation"
html_show_sourcelink = False
html_copy_source = False
html_theme_options = {
    "logo_only": True,
    "navigation_depth": 3,
    "collapse_navigation": False,
    "style_nav_header_background": "#1A1C1F",
}
copybutton_prompt_text = r"\$ |>>> |\.\.\. "
copybutton_prompt_is_regexp = True
