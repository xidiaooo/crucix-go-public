FROM golang:1.22-alpine AS build
WORKDIR /src
COPY src/go.mod ./
COPY src/*.go ./
COPY src/assets ./assets
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/crucix .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/crucix /app/crucix
ENV PORT=8080
EXPOSE 8080
CMD ["/app/crucix"]
