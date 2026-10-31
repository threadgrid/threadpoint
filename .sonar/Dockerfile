# Go must be present on the scan host when SonarQube imports Go coverage.
FROM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS go-toolchain

FROM sonarsource/sonar-scanner-cli:12.3.0.4285_8.1.0@sha256:47956b669bf486bdad919e1cbd0533223515aa0b6d1d31763e5a3754942df6d7

USER root
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
USER scanner-cli
