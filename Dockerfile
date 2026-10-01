# One image for every environment: Compose locally, Docker under systemd on a VM.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /chattie ./cmd/chattie

# Static binary on a minimal base image with no shell, running as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /chattie /chattie
EXPOSE 8080
ENTRYPOINT ["/chattie"]
CMD ["serve"]
