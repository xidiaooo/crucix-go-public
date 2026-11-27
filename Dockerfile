FROM golang:1.22-alpine AS build
WORKDIR /src
COPY src/go.mod ./
COPY src/*.go ./
COPY src/assets ./assets
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/crucix .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl
WORKDIR /app
COPY --from=build /out/crucix /app/crucix
RUN curl -fsSL -o /usr/local/bin/cloudflared https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 && chmod +x /usr/local/bin/cloudflared
COPY entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh
ENV PORT=8080
EXPOSE 8080
CMD ["/app/entrypoint.sh"]
