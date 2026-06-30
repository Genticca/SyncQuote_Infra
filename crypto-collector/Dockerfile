# syntax=docker/dockerfile:1
# crypto-collector (Go) — 수집기 + 아카이버 단일 이미지
# RUN_MODE(all|collector|archiver) 환경변수로 실행 모드를 분리한다.

FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO 비활성(순수 Go) → distroless static에 그대로 올림
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/collector .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/collector /collector
EXPOSE 8080
ENTRYPOINT ["/collector"]
