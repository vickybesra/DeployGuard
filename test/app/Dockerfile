FROM golang:1.24 as builder
WORKDIR /app
COPY main.go .
RUN go mod init testapp && go get github.com/prometheus/client_golang/prometheus && go get github.com/prometheus/client_golang/prometheus/promhttp && CGO_ENABLED=0 go build -o testapp main.go

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/testapp .
EXPOSE 8080
CMD ["./testapp"]
