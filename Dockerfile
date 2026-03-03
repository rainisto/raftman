FROM golang:1.26.0-alpine3.23 AS golang
WORKDIR /src
RUN apk --no-cache add build-base git \
    && GO111MODULE=on go install github.com/mjibson/esc@latest
COPY . ./

RUN go get -u && go generate && go build

FROM alpine:3.23
ENTRYPOINT ["/usr/local/bin/raftman"]
RUN mkdir -p /var/lib/raftman
COPY --from=golang /src/raftman /usr/local/bin/raftman
