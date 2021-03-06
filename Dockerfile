FROM golang:1.10-alpine

RUN apk add --update --no-cache git

RUN mkdir -p /go/src/github.com/influxdata

WORKDIR /go/src/github.com/influxdata

RUN git clone https://github.com/influxdata/influxdb

WORKDIR /go/src/github.com/influxdata/influxdb

RUN git checkout v1.7.7

WORKDIR /go/src/github.com/influxdata

RUN git clone https://github.com/influxdata/telegraf

WORKDIR /go/src/github.com/influxdata/telegraf

RUN git checkout v1.14.3

WORKDIR /go/src/github.com/wavefronthq/

RUN git clone https://github.com/wavefrontHQ/wavefront-sdk-go

WORKDIR /go/src/github.com/wavefronthq/wavefront-sdk-go

RUN git checkout v0.9.1

COPY . /go/src/github.com/mistio/gocky

WORKDIR /go/src/github.com/mistio/gocky

RUN go get

RUN go build

ENTRYPOINT ["/go/src/github.com/mistio/gocky/gocky"]
