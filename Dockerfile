# ------------------------
# stage: build
# ------------------------
FROM golang:1.17.1-buster AS build
WORKDIR /go/src/github.com/mugioka/gim-bot
COPY . ./
RUN go mod download
RUN go build -o main .
# ------------------------
# stage: release
# ------------------------
FROM gcr.io/distroless/base-debian11
COPY --from=build /go/src/github.com/mugioka/gim-bot/main /main
USER nonroot:nonroot
ENTRYPOINT ["/main"]
