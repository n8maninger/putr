FROM docker.io/library/golang:1.23 AS builder

WORKDIR /junkr

# get dependencies
COPY go.mod go.sum ./
RUN go mod download

# copy source
COPY . .
# codegen
RUN go generate ./...
# build
RUN go build -o bin/ -tags='netgo timetzdata' -trimpath -a -ldflags '-s -w -extldflags "-static"'  ./cmd/junkr

FROM scratch

ENV PUID=0
ENV PGID=0

# copy binary and prepare data dir.
COPY --from=builder /junkr/bin/* /usr/bin/
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

USER ${PUID}:${PGID}

ENTRYPOINT [ "junkr" ]