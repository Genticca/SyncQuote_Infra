# Dockerfile — SSE 부하용 커스텀 k6 (xk6-sse 확장 포함)
#
# 기본 grafana/k6 이미지에는 SSE 확장(k6/x/sse)이 없어, xk6로 직접 빌드해 끼워 넣는다.
# (hpa-load-test.js 같은 일반 부하는 기본 grafana/k6 이미지로 충분하다.)
#
# 빌드:
#   docker build -t sq-k6-sse loadtest/web/k6
#
# 실행 (Windows 11 Docker / WSL2):
#   docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
#     sq-k6-sse run -e BASE_URL=http://genti.iptime.org \
#     - < loadtest/web/k6/sse-load-test.js

# 1) xk6로 확장 포함 k6 바이너리 빌드
FROM golang:1.22-bookworm AS build
RUN go install go.k6.io/xk6/cmd/xk6@latest
RUN xk6 build \
    --with github.com/phymbert/xk6-sse@latest \
    --output /k6

# 2) 공식 k6 런타임 이미지에 커스텀 바이너리만 얹는다
#    (ENTRYPOINT는 ["k6"] 그대로 → `docker run sq-k6-sse run ...` 형태로 실행)
FROM grafana/k6:latest
COPY --from=build /k6 /usr/bin/k6
