# Stage 1 — Build the React UI
FROM node:22-alpine AS ui-builder
WORKDIR /build
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ .
RUN npm run build

# Stage 2 — Build the Go binary
FROM golang:1.25 AS go-builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui-builder /internal/api/ui/dist/ internal/api/ui/dist/
RUN CGO_ENABLED=0 go build -o /devops-control ./cmd/devops-control/

# Stage 3 — Minimal runtime with docker CLI
FROM docker:24-cli
RUN apk add --no-cache bash curl gzip python3 openssl py3-jwt py3-cryptography
COPY --from=go-builder /devops-control /usr/local/bin/devops-control
COPY deploy/project.sh /usr/local/bin/deploy/project.sh
COPY deploy/runner/docker-compose.runner.yml /usr/local/bin/deploy/runner/docker-compose.runner.yml
COPY deploy/runner/Dockerfile.runner /usr/local/bin/deploy/runner/Dockerfile.runner
COPY deploy/runner/entrypoint.sh /usr/local/bin/deploy/runner/entrypoint.sh
COPY scripts/ /usr/local/bin/scripts/
EXPOSE 8787
ENTRYPOINT ["devops-control"]
