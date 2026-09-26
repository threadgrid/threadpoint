# Install

Install with Go:

```bash
go install github.com/threadgrid/threadpoint/cmd/threadpoint@latest
```

Or install a released Linux or macOS bundle:

```bash
curl -fsSL https://raw.githubusercontent.com/threadgrid/threadpoint/main/scripts/install.sh | sh
```

The installer places the bundle under `$HOME/.threadpoint/`; the executable is
at `<threadpoint-home>/bin/threadpoint` and is linked into
`$HOME/.local/bin` by default. Pass a release version and command directory
explicitly when needed:

```bash
curl -fsSL https://raw.githubusercontent.com/threadgrid/threadpoint/main/scripts/install.sh | \
  sh -s -- --version v0.1.0 --dir "$HOME/.local/bin"
```

## Add to PATH

If `threadpoint` is not found after installation, add its command directory to
your shell's `PATH`. The installer prints commands when its selected directory
is missing from `PATH`; it does not prompt or edit shell configuration.

For the default installer directory, run the command for your shell:

**Bash or Zsh — current terminal:**

```sh
export PATH="$HOME/.local/bin${PATH:+:$PATH}"
```

**Fish:**

```fish
fish_add_path "$HOME/.local/bin"
```

For future terminals, add the same line once to your shell configuration:

- **Bash:** `~/.bashrc`. For login shells (including macOS terminals using
  Bash), ensure the first existing file among `~/.bash_profile`,
  `~/.bash_login`, and `~/.profile` sources `~/.bashrc`, or put the PATH line
  in that login file as well. If none exists, create `~/.bash_profile`.
- **Zsh:** `${ZDOTDIR:-$HOME}/.zshrc` (normally `~/.zshrc`).
- **Fish:** `~/.config/fish/config.fish`, or
  `$XDG_CONFIG_HOME/fish/config.fish` when `XDG_CONFIG_HOME` is set.
  `fish_add_path` normally persists paths itself; keeping the line in the
  configuration also covers shells using a global `fish_user_paths` and is
  safe to repeat without adding duplicate entries.

Run the command above to update the current terminal. After saving the line,
new terminals will pick it up. Opening a new terminal alone does not configure
PATH. Add a shared command directory only once, even if it holds several tools.

If you used `--dir` or `THREADPOINT_INSTALL_DIR`, substitute that directory
for `$HOME/.local/bin`. For a manual archive install, use the directory where
you placed the executable, rather than the archive or product home.

For `go install`, inspect the configured directories:

```sh
go env GOBIN GOPATH
```

Use `GOBIN` (the first output line) when nonempty. Otherwise use `bin` under
the first `GOPATH` entry (the second output line), normally `$HOME/go/bin`.
Substitute that directory in the shell commands above; Go installs do not use
the installer's `$HOME/.local/bin` default.

Verify from the terminal where you configured PATH:

```sh
threadpoint version
```

## Release verification and policy

See [supply-chain.md](supply-chain.md) for release verification and
[platform-support.md](platform-support.md) for the release matrix.

The installer reads `minimum_install_version`, `blocked_versions`, and
`clipped_versions` from the release policy. Release retention uses
`max_age_days: 90` and `retain_newer_count: 3`; a clipped version remains
unavailable through the installer. Command directories must not contain colons,
which separate PATH entries; invalid directories are rejected before download
or installation.

## Updates and removal

For an installer-managed release, check for and install updates with:

```sh
threadpoint update check
threadpoint update --dir "$HOME/.local/bin"
```

Substitute your selected command directory when using a custom location.
For Go installs, re-run `go install github.com/threadgrid/threadpoint/cmd/threadpoint@latest`.
For manual archives, replace the executable with a verified newer release;
these installs are not managed by `threadpoint update`.

Use `threadpoint uninstall --dir "$HOME/.local/bin" --apply --yes` to remove
only an installer-managed bundle and its command link. It preserves backups,
review state, provider artifacts, and canonical guidance.

The standalone uninstaller is available at
https://raw.githubusercontent.com/threadgrid/threadpoint/main/scripts/uninstall.sh.
It delegates to the managed binary when that binary is healthy. If the bundle
binary is missing or cannot execute, it uses an available Go toolchain to run
the module version selected by `@latest` through Go's checksum-backed module
channel; no sibling `install.sh` is required. This fallback is not
self-contained: it requires a working Go toolchain and either module-network
access or a populated module cache.
