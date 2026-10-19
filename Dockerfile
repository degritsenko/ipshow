FROM golang:1.27.2-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY app/ ./app/

RUN CGO_ENABLED=0 GOOS=linux go build -o server ./app

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /app/server .

RUN mkdir ./data

EXPOSE 8080

CMD ["./server"]
