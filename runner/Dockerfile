FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY params ./params
RUN CGO_ENABLED=0 go build -trimpath -o /out/runner .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/runner /runner
ENTRYPOINT ["/runner"]
