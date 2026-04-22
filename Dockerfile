FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o cutroom ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ffmpeg ca-certificates ttf-dejavu fontconfig && fc-cache -f
WORKDIR /app
COPY --from=builder /app/cutroom .
COPY web/ web/

EXPOSE 8080
CMD ["./cutroom"]
