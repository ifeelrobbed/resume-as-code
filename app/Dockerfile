# syntax=docker/dockerfile:1

FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_TIME=unknown
ARG GIT_REVISION=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.buildTime=${BUILD_TIME} -X main.gitRevision=${GIT_REVISION}" \
    -o /out/app .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/app ./app
COPY templates ./templates
COPY static ./static

EXPOSE 8080
ENTRYPOINT ["./app"]
