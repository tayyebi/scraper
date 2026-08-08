#!/usr/bin/env bash
# Turn Go tool output into GitHub check-run annotations.
#
# Why this exists: nothing in this project is built on the maintainer's machine
# (no Go toolchain, by design). CI is the compiler. Raw Actions logs require
# authentication to read, but check-run annotations on a public repo do not --
# so every diagnostic is re-emitted as a `::error file=,line=::` workflow
# command, making build failures readable over the anonymous REST API.
#
# Reads tool output on stdin, echoes it through verbatim so the log is intact,
# and exits non-zero if it annotated anything.
set -uo pipefail

status=0

emit() {
  # Workflow commands are line-oriented: % and newlines must be escaped or the
  # message is silently truncated at the first special character.
  local msg="$1"
  msg="${msg//%/%25}"
  msg="${msg//$'\r'/%0D}"
  msg="${msg//$'\n'/%0A}"
  printf '%s\n' "$msg"
}

while IFS= read -r line; do
  printf '%s\n' "$line"

  # strip leading whitespace (go test indents subtest diagnostics)
  trimmed="${line#"${line%%[![:space:]]*}"}"

  case "$trimmed" in
    *.go:[0-9]*)
      file="${trimmed%%:*}"
      rest="${trimmed#*:}"
      lineno="${rest%%:*}"
      msg="${rest#*:}"
      case "$lineno" in '' | *[!0-9]*) continue ;; esac
      # drop a column number when the tool emitted file:line:col: msg
      case "$msg" in [0-9]*:*) msg="${msg#*:}" ;; esac
      msg="${msg#"${msg%%[![:space:]]*}"}"
      printf '::error file=%s,line=%s::%s\n' "$file" "$lineno" "$(emit "$msg")"
      status=1
      ;;
    '--- FAIL:'* | 'FAIL'* | *'panic: '*)
      printf '::error::%s\n' "$(emit "$trimmed")"
      status=1
      ;;
  esac
done

exit $status
