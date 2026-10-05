# Coordinator image. Workers normally run on the host next to their browser,
# but this image can also run a worker if a Chromium runtime is provided.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X github.com/unnipv/devdooth/internal/worker.Version=${VERSION}" \
    -o /out/devdooth ./cmd/devdooth

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/devdooth /usr/local/bin/devdooth
# WORKDIR creates /data and makes the default store path usable without a mount.
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/devdooth"]
CMD ["coordinator", "--addr", ":8080", "--store", "/data/devdooth.db"]
