FROM golang:1.22.6-alpine3.20 AS golang
WORKDIR /src
RUN apk --no-cache add build-base git \
    && GO111MODULE=on go install github.com/mjibson/esc@latest
COPY . ./

RUN go get -u && go generate && go build

FROM alpine:3.20
ENTRYPOINT ["/usr/local/bin/raftman"]
RUN mkdir -p /var/lib/raftman
COPY --from=golang /src/raftman /usr/local/bin/raftman
