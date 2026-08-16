# Build from the repository root:
#   docker build -f deploy/Dockerfile -t qrmenu-api .

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are copied first so the module cache layer survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off produces a static binary that runs on distroless/static.
# -trimpath keeps build paths out of the binary; the VCS stamp is what
# /healthz reports as its version.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /api

# Runs as uid 65532; nothing in the image is writable.
USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/api"]
