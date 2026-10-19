for dir in "${ANTIGRAVITY_PLUGIN_ROOT:-}" ${HOME:+"$HOME/.gemini/config/plugins/@NAME@"} ${HOME:+"$HOME/.gemini/antigravity-cli/plugins/@NAME@"}; do
  if [ -n "$dir" ] && [ -f "$dir/bin/anet-mcp.sh" ]; then
    exec sh "$dir/bin/anet-mcp.sh"
  fi
done
echo "The anet plugin directory was not found. Install it with: agy plugin install <directory of the plugin>" >&2
exit 1
