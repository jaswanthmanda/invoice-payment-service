# One image, five binaries (api, worker, psp, seed, webhook-receiver).
# Each compose service picks one with `command`.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=build /out/ /app/
USER app
EXPOSE 8080
CMD ["/app/api"]