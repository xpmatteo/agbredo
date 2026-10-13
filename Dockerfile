FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agbredo ./cmd/agbredo

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/agbredo /app/agbredo
EXPOSE 8080
ENTRYPOINT ["/app/agbredo"]
CMD ["serve"]
