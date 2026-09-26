# Network Egress and No Telemetry

Threadpoint is local-first. The shipped layout, stage, commit, prune,
restore, and status commands process files on the machine and do not send
project content to a service.

Network access from shipped commands is limited to explicit distribution
actions:

- installer downloads and release verification;
- `threadpoint update` release metadata and release artifacts.

The maintainer-only `make sonar` target is separate from the shipped CLI. When
explicitly invoked with analysis credentials, it uploads the tracked revision,
SCM metadata, and generated coverage to `https://sonarcloud.io`. Ordinary
commands, tests, and builds do not invoke that service.

No command emits usage analytics, events, project content, or diagnostic
reports as telemetry and does not create ambient local diagnostic logs. Users
remain responsible for reviewing command output before sharing it.
