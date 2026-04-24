FROM golang:1.23-alpine AS builder

WORKDIR /app

RUN apk --no-cache add git

COPY go.mod ./
RUN go mod download

COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o sonora-server .

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/sonora-server .
EXPOSE 8080
CMD ["./sonora-server"]
