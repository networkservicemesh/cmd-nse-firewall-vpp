ARG VPP_VERSION=v22.02-rc0-165-gb44d0defb
FROM ghcr.io/edwarnicke/govpp/vpp:${VPP_VERSION} as go
COPY --from=golang:1.16.3-buster /usr/local/go/ /go
ENV PATH ${PATH}:/go/bin
ENV GO111MODULE=on
ENV CGO_ENABLED=0
ENV GOBIN=/bin
RUN rm -r /etc/vpp
RUN go get github.com/go-delve/delve/cmd/dlv@v1.6.0
RUN go get github.com/edwarnicke/dl
RUN dl https://github.com/spiffe/spire/releases/download/v0.11.1/spire-0.11.1-linux-x86_64-glibc.tar.gz | \
    tar -xzvf - -C /bin --strip=3 ./spire-0.11.1/bin/spire-server ./spire-0.11.1/bin/spire-agent

FROM go as build
WORKDIR /build
COPY go.mod go.sum ./
COPY ./internal/imports ./internal/imports
RUN go build ./internal/imports
COPY . .
RUN go build -o /bin/app .

FROM build as test
CMD go test -test.v ./...

FROM test as debug
CMD dlv -l :40000 --headless=true --api-version=2 test -test.v ./...

FROM ghcr.io/edwarnicke/govpp/vpp:${VPP_VERSION} as runtime
COPY --from=build /bin/app /bin/app
ENTRYPOINT [ "/bin/app" ]
