FROM golang:alpine AS builder
RUN apk add --no-cache build-base
COPY . /dane
WORKDIR /dane/cmd/letsdane
RUN go build

FROM alpine:latest 
RUN apk add --no-cache unbound-libs
COPY --from=builder /dane /dane
WORKDIR /dane/cmd/letsdane
EXPOSE 38080
ENTRYPOINT ["./letsdane"]
