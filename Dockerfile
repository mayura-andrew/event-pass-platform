FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/event-pass-api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -o /out/event-pass-migrate ./cmd/migrate

FROM alpine:3.20

RUN adduser -D -H -u 10001 app
WORKDIR /app
COPY --from=build /out/event-pass-api /app/event-pass-api
# Applies migrations/ (embedded): /app/event-pass-migrate up
COPY --from=build /out/event-pass-migrate /app/event-pass-migrate
COPY public /app/public

USER app
EXPOSE 8080
ENTRYPOINT ["/app/event-pass-api"]
