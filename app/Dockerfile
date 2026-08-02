# syntax=docker/dockerfile:1

FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.buildTime=${BUILD_TIME}" \
    -o /out/app .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/app ./app
COPY templates ./templates
# No static/ dir yet - add a COPY here once real assets (favicon, etc.) exist.

EXPOSE 8080
ENTRYPOINT ["./app"]
