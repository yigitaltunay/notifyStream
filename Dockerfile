# syntax=docker/dockerfile:1

FROM golang:1.23-bookworm AS build
WORKDIR /src

ENV GOTOOLCHAIN=auto

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
	&& CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
	&& CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/scheduler ./cmd/scheduler

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/api /app/api
COPY --from=build /out/worker /app/worker
COPY --from=build /out/scheduler /app/scheduler
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT []
CMD ["/app/api"]
