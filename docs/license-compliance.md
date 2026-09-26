# License Compliance

Threadpoint is distributed under the Apache License, Version 2.0. The source
repository carries the project `LICENSE` and `NOTICE` files, and release archives
must include both files with the released binary.

## Release Requirements

Before publishing a release, maintainers must run the dependency license check:

```bash
scripts/check-licenses.sh
```

The script uses `go-licenses` v2 and includes test imports so fixture and helper
dependencies are reviewed before release. CI also runs this check and can emit a
`licenses.csv` report as a build artifact.

Release archives include:

- `<archive-stem>/bin/threadpoint` binary;
- `<archive-stem>/README.md`;
- `<archive-stem>/LICENSE`;
- `<archive-stem>/NOTICE`.

That four-file payload, wrapped under a single top-level archive-stem directory,
is the stable initial-release archive contract. `NOTICE` is bundled intentionally even when
current third-party notice content is minimal, because Apache-2.0
redistribution and installer safety checks both depend on the documented
archive shape.

## Dependency Review

New Go module dependencies must have licenses compatible with Apache-2.0 source
and binary redistribution. The default allowlist is intentionally narrow:
Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, and ISC.

If a dependency requires attribution, notice preservation, source distribution,
or another redistribution condition, update `NOTICE` or the release process
before merging the dependency. Unknown, forbidden, reciprocal, noncommercial, or
custom licenses require maintainer review before they can be accepted.

## Downstream Embedding And Redistribution

Downstream software that embeds threadpoint source, links threadpoint packages,
or redistributes binaries containing threadpoint code is responsible for meeting
Apache-2.0 redistribution obligations for its own distribution.

At minimum, redistributed binary or source packages that include threadpoint
code should preserve threadpoint's Apache-2.0 `LICENSE` and `NOTICE` materials
alongside the downstream project's own notices. If a downstream package has a
combined notice file, include threadpoint's notice text there without removing
existing downstream or third-party notices.

## Generated Reports

Generated dependency license reports are CI and release-check artifacts for the initial release.
They are not committed to the repository by default. If a future dependency adds
a required third-party notice, the durable source of truth is `NOTICE`, not an
unreviewed generated report. Generated reports such as `licenses.csv` are not
bundled into initial release archives.

## Current State

The current Go module directly requires `golang.org/x/sys` for native,
destination-no-replace filesystem operations on supported operating systems.
That module is distributed under the BSD 3-Clause license. Its Go Authors
copyright, redistribution conditions, and disclaimer are preserved in the
project `NOTICE` so source and binary release archives carry the attribution
required by the dependency's license.
