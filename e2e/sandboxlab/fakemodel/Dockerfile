# The scripted Anthropic-compatible model stand-in (see main.go). Stdlib
# only, so a bare Go builder suffices — no woca module context needed.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY main.go .
RUN go build -trimpath -o /fakemodel main.go

FROM alpine:3.22
COPY --from=build /fakemodel /usr/local/bin/fakemodel
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fakemodel"]
