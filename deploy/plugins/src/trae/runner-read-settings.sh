# read_settings prints two lines, anetPath and identity, from the file
# "settings" that install.sh of this plugin writes in the directory above bin.
# The lines of the file are "anetPath=<value>" and "identity=<value>"; the file
# is data and is never executed. Without the file, both settings are empty.
read_settings() {
  settings_file=$(dirname "$0")/../settings
  settings_bin=
  settings_id=
  if [ -r "$settings_file" ]; then
    settings_bin=$(sed -n 's/^anetPath=//p' "$settings_file" | sed -n 1p)
    settings_id=$(sed -n 's/^identity=//p' "$settings_file" | sed -n 1p)
  fi
  printf '%s\n%s\n' "$settings_bin" "$settings_id"
}
