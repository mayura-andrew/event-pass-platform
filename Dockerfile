FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/nsac-qr-api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -o /out/nsac-qr-migrate ./cmd/migrate

FROM alpine:3.20

RUN adduser -D -H -u 10001 app
WORKDIR /app
COPY --from=build /out/nsac-qr-api /app/nsac-qr-api
# Applies migrations/ (embedded): /app/nsac-qr-migrate up
COPY --from=build /out/nsac-qr-migrate /app/nsac-qr-migrate
COPY public /app/public

USER app
EXPOSE 8080
ENTRYPOINT ["/app/nsac-qr-api"]
