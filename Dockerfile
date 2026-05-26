FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o money-track main.go

FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/money-track .
COPY --from=builder /app/static ./static
EXPOSE 8080
CMD ["./money-track"]
