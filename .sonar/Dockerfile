# Go must be present on the scan host when SonarQube imports Go coverage.
FROM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS go-toolchain

FROM sonarsource/sonar-scanner-cli:12.4.0.4321_8.4.0@sha256:6e6eca4d552695a98fb7dab84f0c1bd0885ebbfe272654d1552e24bd7f452d51

USER root
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
USER scanner-cli
