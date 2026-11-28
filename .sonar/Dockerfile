# Go must be present on the scan host when SonarQube imports Go coverage.
FROM golang:1.26.11-bookworm@sha256:ea47ee45f1284d60f9cc09cb2c3bb4f508c256b640bd8bb30342ff13d1adcb52 AS go-toolchain

FROM sonarsource/sonar-scanner-cli:12.5.0.4351_8.5.0@sha256:8bb84373f00346f9a579adb7ccb2999f8cd050ab7167fa9ae08a16069de03c84

USER root
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
USER scanner-cli
