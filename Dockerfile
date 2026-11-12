FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kube-fence ./cmd/kube-fence

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kube-fence /kube-fence
USER 65532:65532
ENTRYPOINT ["/kube-fence"]
