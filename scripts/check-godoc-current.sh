#!/bin/sh
# Generate or check godoc-current.txt: the committed snapshot of `go doc -all`
# for every public package in this module. CI runs this without --update and

set -eu

# Canonicalize docs to Linux so macOS hooks match CI.
export GOOS=linux

gen() {
  # Substitution drops trailing newlines and printf restores exactly one, so
  # this generator and end-of-file-fixer agree on the file they share.
  body=$(
    for pkg in $(go list ./... | grep -v '/examples/'); do
      echo "## ${pkg}"
      echo
      go doc -all "${pkg}"
      echo
    done
  )
  printf '%s\n' "$body"
}

case "${1:-}" in
  --update)
    gen > godoc-current.txt
    echo "godoc-current.txt updated"
    ;;
  *)
    if ! gen | diff -u godoc-current.txt -; then
      echo
      echo "godoc-current.txt is out of date." >&2
      echo "Regenerate with: ./scripts/check-godoc-current.sh --update" >&2
      echo "Or: just godoc-update" >&2
      exit 1
    fi
    ;;
esac
