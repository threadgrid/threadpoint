# Go must be present on the scan host when SonarQube imports Go coverage.
FROM golang:1.26.8-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS go-toolchain

FROM sonarsource/sonar-scanner-cli:12.2.0.4256_8.1.0@sha256:a3f4215076706c95a17a68c19322ee916e40a3acd081a8c1a1e839e0194afa57

USER root
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
USER scanner-cli
