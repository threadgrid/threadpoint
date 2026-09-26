# Supported Platforms

The initial threadpoint release supports Linux and macOS on amd64 and arm64.

The initial release matrix is intentionally narrow: every published release artifact
should map to a documented support target, and every supported target should be
covered by release-blocking confidence before tagging.

| Operating system | Architecture | Release artifact                        | Install methods                                 | initial-release status |
| ---------------- | ------------ | --------------------------------------- | ----------------------------------------------- | ---------------------- |
| Linux            | amd64        | `threadpoint_0.1.0_linux_amd64.tar.gz`  | `go install`, release archive, installer script | release-blocking       |
| Linux            | arm64        | `threadpoint_0.1.0_linux_arm64.tar.gz`  | `go install`, release archive, installer script | release-blocking       |
| macOS            | amd64        | `threadpoint_0.1.0_darwin_amd64.tar.gz` | `go install`, release archive, installer script | release-blocking       |
| macOS            | arm64        | `threadpoint_0.1.0_darwin_arm64.tar.gz` | `go install`, release archive, installer script | release-blocking       |

## Install Method Policy

- `go install` is the default install method for all supported initial-release targets.
- Release archives are published for all supported initial-release targets and verified with
  the release `checksums.txt` file.
- The installer script targets the same Linux and macOS matrix.
- Package-manager channels are deferred until after the first public foundation
  release.

## Shell And Path Assumptions

- Installer-script flows assume a POSIX shell on Linux and macOS.
- Release archives contain the `threadpoint` binary and should be extracted into
  a user-selected directory on `PATH`.
- Threadpoint does not edit shell profiles or login startup files as part of the initial-release install policy.
- WSL is treated as Linux for the initial release binaries. A project stored on a mounted Windows
  filesystem such as `/mnt/c/...` is supported only when explicitly selected as
  `--root`; threadpoint does not use WSL as permission to scan Windows
  home-directory provider stores outside that root.

## Deferred Platforms

Windows is deferred for the initial release. Threadpoint does not publish Windows release
artifacts, provide a Windows installer path, or claim Windows support until a
dedicated Windows distribution path is defined.

Windows-native release artifacts, Windows path semantics, `%USERPROFILE%`,
`AppData`, and Windows-native provider-home discovery remain outside the initial-release support promise. Unsupported operating systems, architectures, shells,
and package managers may work from source, but they are outside that promise.
