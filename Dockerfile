FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/ballcoach-media .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=build /bin/ballcoach-media /bin/ballcoach-media
EXPOSE 8080
ENTRYPOINT ["/bin/ballcoach-media"]
