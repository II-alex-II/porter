# syntax=docker/dockerfile:1.1.7-experimental

# Base Go environment
# -------------------
FROM golang:1.17-alpine as base
WORKDIR /porter

RUN apk update && apk add --no-cache gcc musl-dev git

COPY go.mod go.sum ./
COPY /cmd ./cmd
COPY /internal ./internal
COPY /api ./api
COPY /ee ./ee

RUN --mount=type=cache,target=$GOPATH/pkg/mod \
    go mod download

# Go build environment
# --------------------
FROM base AS build-go

ARG version=production

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=$GOPATH/pkg/mod \
    go build -ldflags="-w -s -X 'main.Version=${version}'" -tags ee -a -o ./bin/app ./cmd/app && \
    go build -ldflags '-w -s' -a -tags ee -o ./bin/migrate ./cmd/migrate && \
    go build -ldflags '-w -s' -a -tags ee -o ./bin/ready ./cmd/ready

# Go test environment
# -------------------
FROM base AS porter-test

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=$GOPATH/pkg/mod \
    go test ./...

# Webpack build environment
# -------------------------
FROM node:16 as build-webpack
WORKDIR /webpack

COPY ./dashboard ./

RUN npm install -g npm@8.1

RUN npm i --legacy-peer-deps

RUN npm run build

# Deployment environment
# ----------------------
FROM alpine
RUN apk update

COPY --from=build-go /porter/bin/app /porter/
COPY --from=build-go /porter/bin/migrate /porter/
COPY --from=build-go /porter/bin/ready /porter/
COPY --from=build-webpack /webpack/build /porter/static

ENV DEBUG=false
ENV STATIC_FILE_PATH=/porter/static
ENV SERVER_PORT=8080
ENV SERVER_TIMEOUT_READ=5s
ENV SERVER_TIMEOUT_WRITE=10s
ENV SERVER_TIMEOUT_IDLE=15s

ENV COOKIE_SECRETS=secret

ENV SQL_LITE=true
ENV ADMIN_INIT=false

EXPOSE 8080
CMD /porter/migrate && /porter/app
