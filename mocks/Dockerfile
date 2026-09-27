FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go *.html ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/mocks .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mocks /mocks
EXPOSE 8080
ENTRYPOINT ["/mocks"]
