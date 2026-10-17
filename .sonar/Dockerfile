# Go must be present on the scan host when SonarQube imports Go coverage.
FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS go-toolchain

FROM sonarsource/sonar-scanner-cli:12.1.0.3233_8.0.1@sha256:23ca0f137965d9dff2198074043fd48d386280bc5d0ccac8c8349cea4cf096a9

USER root
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
USER scanner-cli
