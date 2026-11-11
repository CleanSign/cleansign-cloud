# Stage 1: build
FROM golang:1.27-alpine AS build
WORKDIR /src

# Download dependencies first so the cache survives source changes
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# Stage 2: run. Contains only the single binary (migration SQL is embedded in the binary)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 4000
ENTRYPOINT ["/server"]
