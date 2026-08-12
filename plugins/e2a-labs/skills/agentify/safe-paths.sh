#!/usr/bin/env bash
set -euo pipefail

safe_stderr() {
  printf '%s\n' "$*" >&2
}

safe_die() {
  safe_stderr "agentify: $*"
  exit 1
}

safe_paths_init() { # target-root
  local target_root=$1 canonical
  [ -z "${SAFE_TARGET_ROOT+x}" ] || safe_die "safe target root is already initialized"
  [ -d "$target_root" ] || safe_die "target root is not an existing directory: $target_root"
  canonical=$(cd -- "$target_root" && pwd -P)
  readonly SAFE_TARGET_ROOT="$canonical"
}

safe_validate_relative() { # relative-path
  local rel=$1 component
  local -a components
  case "$rel" in
    ""|/*|*/|*//*) safe_die "invalid relative path: $rel" ;;
  esac
  IFS='/' read -r -a components <<< "$rel"
  for component in "${components[@]}"; do
    case "$component" in
      ""|.|..) safe_die "invalid relative path: $rel" ;;
    esac
  done
}

safe_mkdir() { # relative-directory
  local rel=$1 current component
  local -a components
  [ "$rel" != "." ] || return 0
  safe_validate_relative "$rel"
  current=$SAFE_TARGET_ROOT
  IFS='/' read -r -a components <<< "$rel"
  for component in "${components[@]}"; do
    current="$current/$component"
    [ ! -L "$current" ] || safe_die "symbolic-link directory: $rel"
    if [ -e "$current" ]; then
      [ -d "$current" ] || safe_die "non-directory path component: $rel"
    else
      mkdir -- "$current"
    fi
  done
}

safe_existing_file() { # relative-file
  local rel=$1 parent dest
  safe_validate_relative "$rel"
  parent=${rel%/*}; [ "$parent" = "$rel" ] && parent=.
  safe_mkdir "$parent"
  dest="$SAFE_TARGET_ROOT/$rel"
  [ ! -L "$dest" ] || safe_die "symbolic-link destination: $rel"
  if [ -e "$dest" ]; then
    [ -f "$dest" ] || safe_die "non-regular destination: $rel"
    printf '%s\n' "$dest"
    return 0
  fi
  return 1
}

safe_write() { # relative-file mode; complete content on stdin
  local rel=$1 mode=$2 parent dest temporary
  safe_validate_relative "$rel"
  parent=${rel%/*}; [ "$parent" = "$rel" ] && parent=.
  safe_mkdir "$parent"
  dest="$SAFE_TARGET_ROOT/$rel"
  [ ! -L "$dest" ] || safe_die "symbolic-link destination: $rel"
  [ ! -e "$dest" ] || [ -f "$dest" ] || safe_die "non-regular destination: $rel"
  temporary=$(mktemp "$SAFE_TARGET_ROOT/$parent/.agentify.tmp.XXXXXX")
  trap 'rm -f -- "$temporary"' RETURN
  cat > "$temporary"
  chmod "$mode" "$temporary"
  mv -f -- "$temporary" "$dest"
  trap - RETURN
}

safe_copy() { # source-file relative-file mode
  local source=$1 rel=$2 mode=$3
  [ -f "$source" ] || safe_die "template is not a regular file: $source"
  safe_write "$rel" "$mode" < "$source"
}
