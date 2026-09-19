# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /hackathon-webhook .

# Distroless static: CA certificates for the outbound HTTPS calls, a non-root
# user, and no shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hackathon-webhook /hackathon-webhook
EXPOSE 8080
ENTRYPOINT ["/hackathon-webhook"]
