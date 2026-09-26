#!/usr/bin/env sh
set -eu

PROFILE=${1:?usage: check-coverage.sh COVERPROFILE}

if [ ! -r "$PROFILE" ]; then
  printf 'Go coverage gate failed: profile is not readable: %s\n' "$PROFILE" >&2
  exit 1
fi

awk '
  NR == 1 {
    if ($1 != "mode:" || NF != 2) {
      invalid = 1
    }
    next
  }
  NF != 3 {
    invalid = 1
    next
  }
  {
    # go test -coverpkg writes every instrumented source block for each test
    # binary. Coalesce identical blocks and retain their greatest hit count so
    # a package is measured once across the complete module test suite.
    block = $1 " " $2
    if (!(block in statements)) {
      statements[block] = $2
      hits[block] = $3
    } else if ($3 > hits[block]) {
      hits[block] = $3
    }
  }
  END {
    for (block in statements) {
      split(block, fields, " ")
      package = fields[1]
      sub(/:[^:]*$/, "", package)
      if (package !~ /\//) package = "."
      else sub(/\/[^/]*$/, "", package)
      packageTotal[package] += statements[block]
      total += statements[block]
      if (hits[block] > 0) {
        covered += statements[block]
        packageCovered[package] += statements[block]
      }
    }
    if (invalid || total == 0) {
      printf "Go coverage gate failed: profile has no valid statement data\n" > "/dev/stderr"
      exit 2
    }
    percent = (covered * 100) / total
    printf "Go statement coverage: %.2f%% (%d/%d statements)\n", percent, covered, total
    for (package in packageTotal) {
      printf "Go package coverage %s: %.2f%% (%d/%d statements)\n", package, packageCovered[package] * 100 / packageTotal[package], packageCovered[package], packageTotal[package]
      if (packageCovered[package] * 100 < packageTotal[package] * 85) failed = 1
    }
    if (failed || covered * 100 < total * 85) {
      printf "Go coverage gate failed: requires at least 85.00%% statement coverage in the module and every package\n" > "/dev/stderr"
      exit 1
    }
  }
' "$PROFILE"
