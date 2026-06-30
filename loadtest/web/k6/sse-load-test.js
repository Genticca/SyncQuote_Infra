// sse-load-test.js — SyncQuote SSE 스트리밍 부하 테스트 (k6 + xk6-sse 확장)
//
// 왜 기본 http.get이 아니라 확장(k6/x/sse)인가:
//   기본 http.get은 "응답을 끝까지 받아야 끝나는" 모델이라 스트리밍(text/event-stream)을 못 다룬다.
//   이 서버는 SseEmitter(Long.MAX_VALUE)로 연결을 무한 유지하므로, http.get은 매 요청이
//   타임아웃까지 블로킹된 뒤 status 0(실패)로만 찍혀 측정이 무의미하다.
//   xk6-sse는 SSE를 네이티브로 열어 이벤트를 실제 수신하며 연결을 유지하므로,
//   서버 timeout 정책(무한/유한)과 무관하게 동작한다.
//
// ⚠️ 이 스크립트는 확장(k6/x/sse)이 포함된 커스텀 k6가 필요하다.
//    같은 디렉토리의 Dockerfile로 이미지를 빌드해 실행한다(README §2-3).
//      docker build -t sq-k6-sse loadtest/web/k6
//
// 무엇을 재는가(SSE 부하 = "동시에 열려있는 연결 수 × 유지 시간"):
//   sse_opened                 수립된 연결 수
//   sse_events                 수신한 SSE 이벤트 총수
//   sse_errors                 연결 중 발생한 에러(끊김) 수
//   sse_time_to_first_event    연결 후 최초 이벤트까지 지연(ms)
//
// 실행 예 (Windows 11 Docker / WSL2):
//   docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
//     sq-k6-sse run -e BASE_URL=http://genti.iptime.org \
//     - < loadtest/web/k6/sse-load-test.js
//
//   더 많은 동시 연결:
//   docker run --rm -i --network host --ulimit nofile=1048576:1048576 \
//     sq-k6-sse run -e BASE_URL=http://genti.iptime.org -e MAX_CONN=600 \
//     - < loadtest/web/k6/sse-load-test.js
//
// ⚠️ DDNS(genti.iptime.org) 경로로 대량 동시연결을 걸면 가정용 공유기의 conntrack/NAT
//    세션 테이블이 먼저 고갈돼 집 인터넷이 끊길 수 있다. 작게 시작해 공유기 세션 수를
//    함께 관찰하라(README §4). 동시연결 한계만 깨끗이 보려면 내부에서 노드 직접 호출 권장.

import sse from 'k6/x/sse';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const BASE_URL = (__ENV.BASE_URL || 'http://localhost:80').replace(/\/$/, '');
const MAX_CONN = parseInt(__ENV.MAX_CONN || '300', 10);
if (Number.isNaN(MAX_CONN) || MAX_CONN <= 0) {
    throw new Error(`MAX_CONN은 1 이상의 정수여야 합니다: "${__ENV.MAX_CONN}"`);
}
// 실엔드포인트(검증됨): /api/v1/stream/tickers — exchange, markets 쿼리는 선택(없으면 전체 구독).
const STREAM_PATH = __ENV.STREAM_PATH || '/api/v1/stream/tickers';

const opened = new Counter('sse_opened');
const events = new Counter('sse_events');
const errors = new Counter('sse_errors');
const ttfe = new Trend('sse_time_to_first_event', true);

export const options = {
    scenarios: {
        // RPS가 아니라 "동시 연결(VU)을 높게 잡고 길게 유지"가 핵심.
        // 각 VU가 SSE 연결 1개를 stage 동안 그대로 붙잡고, ramp-down(target:0)에서 정리한다.
        // 수동 close가 없어 서버가 무한 유지해도 안전하다.
        sse_concurrency: {
            executor: 'ramping-vus',
            startVUs: 0,
            stages: [
                { duration: '1m', target: Math.round(MAX_CONN / 3) },
                { duration: '2m', target: Math.round(MAX_CONN / 3) },
                { duration: '1m', target: MAX_CONN },
                { duration: '3m', target: MAX_CONN },   // 메모리/FD/세션 한계 관찰 구간
                { duration: '1m', target: 0 },          // 연결 정리(scale-in 관찰)
            ],
            gracefulRampDown: '30s',
        },
    },
};

export default function () {
    const url = `${BASE_URL}${STREAM_PATH}`;
    const start = Date.now();
    let gotFirst = false;

    const res = sse.open(url, { method: 'GET', tags: { name: 'sse_stream' } }, function (client) {
        client.on('open', function () {
            opened.add(1);
        });
        client.on('event', function () {
            if (!gotFirst) {
                ttfe.add(Date.now() - start);
                gotFirst = true;
            }
            events.add(1);
        });
        client.on('error', function () {
            errors.add(1);
        });
        // close() 호출 안 함 → ramping-vus가 동시 연결을 stage 동안 유지/정리.
    });

    check(res, { 'SSE 연결 수립(200)': (r) => r && r.status === 200 });
}
