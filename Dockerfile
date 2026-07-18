# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.26 AS build

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Statically-linked, stripped, reproducible binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/server ./cmd/server

# ---- Runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/server /server

EXPOSE 9090
USER nonroot:nonroot

ENTRYPOINT ["/server"]
