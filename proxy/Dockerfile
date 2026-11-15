# --- build stage ---
FROM golang:1.27-alpine AS build
WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off -> fully static binary that runs on an empty base image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mysql-proxy ./cmd/server

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mysql-proxy /mysql-proxy
EXPOSE 3308
# Exec form: the proxy is PID 1 and receives SIGTERM from `docker stop`
# directly, which triggers its graceful shutdown.
ENTRYPOINT ["/mysql-proxy"]
CMD ["-addr", ":3308"]
