FROM ubuntu:focal AS builder

ENV DEBIAN_FRONTEND noninteractive
ENV GOLANG_VERSION 1.21

RUN apt-get update && \
    apt-get install -y git wget tar curl sudo tcpdump && \
    apt-get clean


RUN wget https://dl.google.com/go/go1.22.5.linux-amd64.tar.gz && tar -xvf go1.22.5.linux-amd64.tar.gz && mv go /usr/local
ENV GOROOT=/usr/local/go
RUN mkdir goproject
ENV GOPATH=/goproject
ENV PATH=$GOPATH/bin:$GOROOT/bin:$PATH

RUN git clone https://github.com/AbdallahRustom/chf.git /chf && cd /chf && \
    git checkout Feature/HeartBeat

WORKDIR /chf
RUN go mod tidy
RUN go build -o ./chf ./cmd/main.go

FROM alpine:3.15

LABEL description="Free5GC open source 5G Core Network" \
    version="Stage 3"

ENV F5GC_MODULE chf
ARG DEBUG_TOOLS

# Install debug tools ~ 100MB (if DEBUG_TOOLS is set to true)
RUN if [ "$DEBUG_TOOLS" = "true" ] ; then apk add -U vim strace net-tools curl netcat-openbsd ; fi

# Set working dir
WORKDIR /free5gc
RUN mkdir -p config/ log/ cert/

# Copy executable and default certs
COPY --from=builder /chf ./
COPY  ./cert/${F5GC_MODULE}.pem ./cert/
COPY ./cert/${F5GC_MODULE}.key ./cert/

# Config files volume
VOLUME [ "/free5gc/config" ]

# Certificates (if not using default) volume
VOLUME [ "/free5gc/cert" ]

# Exposed ports
EXPOSE 7777