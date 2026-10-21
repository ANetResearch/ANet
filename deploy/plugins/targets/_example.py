"""A minimal example target for adapter authors: copy this file to targets/<key>.py.

A module whose name starts with an underscore is skipped by the generator
unless it is named explicitly:

    python3 deploy/plugins/build.py --target _example --out /tmp/example

The example models a host that

  * reads a manifest .example-plugin/plugin.json and an .mcp.json at the
    plugin root,
  * expands ${EXAMPLE_PLUGIN_ROOT} in the MCP configuration and in skills,
  * has no settings store, so that the two anet settings arrive as the
    environment variables ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY, and
  * reads a marketplace catalogue .example-plugin/marketplace.json from the
    root of a repository.

The interface is described in README.md ("The adapter interface").
"""

# Required. The human name of the host; it is also the value of the variable TOOL
# in the shared texts ("Set up anet for Example").
TITLE = "Example"

# Optional. One line for the table in the README of the output root.
HELP = "Example host (documentation of the adapter interface; not built by default)."

# Optional. Directory of the website tree (--site) for this target. Default: plugins/<key>.
# SITE_PATH = "plugins/example"

# Optional. Strings that occur only in this target's own files. The generator fails
# when a file of another target contains one, which catches template leaks.
MARKERS = ["EXAMPLE_PLUGIN_ROOT"]

# Required: the template variables of this target. Values are text, True/False
# (conditions), or {"en": ..., "zh-CN": ...} for text that differs by language.
# The three below must be present; the others have defaults (README.md).
VARS = {
    "TOOL": "Example",
    # How a skill and a command reach bin/anet-run.sh. Target syntax, any form of text.
    "RUNNER_FROM_SKILL": "${EXAMPLE_PLUGIN_ROOT}/bin/anet-run.sh",
    "RUNNER_FROM_COMMAND": "${EXAMPLE_PLUGIN_ROOT}/bin/anet-run.sh",
    # Optional, with their defaults:
    # "SETTINGS_STORE": False,   # True when the host keeps the plugin settings in a file
    # "HAS_COMMAND": True,       # False when the host has no slash commands
    # "DOC_SLUG": "",            # docs/agents/<slug>.rst of the anet documentation
}


def build(ctx):
    """Required. Emit the files of the plugin with ctx.add*; see README.md."""
    meta = ctx.meta
    ctx.add_common()  # bin/, skills/, commands/, README.md, README_CN.md, LICENSE
    ctx.add_json(".example-plugin/plugin.json", {
        "name": meta["name"],
        "version": meta["version"],
        "description": meta["description"],
    })
    ctx.add_json(".mcp.json", {
        "mcpServers": {
            meta["mcp"]["server_name"]: {
                # For a host that spawns with cross-spawn and keeps the executable bit: bin/anet-mcp on
                # Linux and macOS, bin/anet-mcp.cmd on Windows. Verify both for each host and say how
                # in the docstring of the module.
                "command": "${EXAMPLE_PLUGIN_ROOT}/bin/anet-mcp",
                "args": [],
                # No settings store: pass the values the user configured in the host.
                "env": {s["env"]: "${%s}" % s["env"] for s in meta["settings"]},
            }
        }
    })
    # A file at the root of the output (the mirror repository, or dist/ in the ANet repository).
    ctx.add_root_json(".example-plugin/marketplace.json", {
        "name": "agentnetwork",
        "plugins": [{"name": meta["name"], "source": f"./{ctx.key}"}],
    })


def site(ctx, site):
    """Optional. Emit the files that are served over HTTPS (see README.md)."""
    archive = site.archive(f"{ctx.meta['name']}-{ctx.meta['version']}.zip", wrapper=ctx.meta["name"])
    site.add_json("marketplace.json", {
        "name": "agentnetwork",
        "plugins": [{"name": ctx.meta["name"], "url": archive["url"], "sha256": archive["sha256"]}],
    })
