// hpa-load-test.js — SyncQuote Query API 부하 테스트 (k6, 통합본)
//
// 목적: 클러스터 외부의 별도 서버에서 Query API(읽기 경로)에 지속적인 동시 요청을
//   흘려보내 CPU를 끌어올리고, HPA의 scale-out/scale-in 동작을 검증한다.
//
// [이 통합본의 구성]
//   - 베이스: setup()에서 /api/v1/markets로 실데이터를 받아 부하 (하드코딩 X)
//             responseType:'none'으로 생성기 메모리 절약 / candles 1s(Redis)·1m(MySQL) 경로 분리
//   - 추가  : 단일 노드(maxReplicas:4) 결과 해석 가이드 (README), metrics-server 사전 체크 (README)
//   - SSE   : 스트리밍 부하는 성격이 달라 별도 파일(sse-load-test.js)로 분리
//
// 설치: https://k6.io/docs/get-started/installation/  (또는 docker run grafana/k6)
//
// 실행 예:
//   k6 run -e BASE_URL=http://<클러스터 외부 노출 주소>:<port> loadtest/web/k6/hpa-load-test.js
//
//   더 강한 부하로 6분 유지:
//   k6 run -e BASE_URL=http://1.2.3.4:30080 -e MAX_VUS=400 -e RAMP_UP=2m -e HOLD=6m -e RAMP_DOWN=3m \
//     loadtest/web/k6/hpa-load-test.js
//
// 옵션(-e KEY=VALUE):
//   BASE_URL   대상 Query API 베이스 URL (필수, 기본값은 로컬 테스트용)
//   MAX_VUS    최대 동시 가상 사용자 수 (기본 200)
//   RAMP_UP    0 -> MAX_VUS 증가 시간, scale-out 유발 구간 (기본 3m)
//   HOLD       MAX_VUS 유지 시간, CPU 70% 초과 구간 (기본 5m)
//   RAMP_DOWN  MAX_VUS -> 0 감소 시간, scale-in 관찰 구간 (기본 3m)

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';

const BASE_URL = (__ENV.BASE_URL || 'http://localhost:80').replace(/\/$/, '');
const MAX_VUS = parseInt(__ENV.MAX_VUS || '200', 10);
if (Number.isNaN(MAX_VUS) || MAX_VUS <= 0) {
    throw new Error(`MAX_VUS는 1 이상의 정수여야 합니다: "${__ENV.MAX_VUS}"`);
}
const RAMP_UP = __ENV.RAMP_UP || '3m';
const HOLD = __ENV.HOLD || '5m';
const RAMP_DOWN = __ENV.RAMP_DOWN || '3m';

const EXCHANGES = ['UPBIT', 'BINANCE'];
const INTERVALS = ['1s', '1m']; // 1s = Redis 경로, 1m = MySQL 경로

const tickerErrors = new Counter('ticker_errors');
const candleErrors = new Counter('candle_errors');

export const options = {
    scenarios: {
        hpa_scale_demo: {
            executor: 'ramping-vus',
            startVUs: 0,
            stages: [
                { duration: RAMP_UP, target: MAX_VUS }, // scale-out 유발 구간
                { duration: HOLD, target: MAX_VUS },    // CPU 70% 초과 유지 구간
                { duration: RAMP_DOWN, target: 0 },     // scale-in 관찰 구간
            ],
            gracefulRampDown: '30s',
        },
    },
    // 의도적으로 과부하를 거는 테스트라 기본적으로 http_req_failed 등에 임계치를 두지 않는다.
    // HOLD 구간에서 실패율이 올라가는 것 자체가 "한계 도달"이라는 정상 신호일 수 있고,
    // 임계치를 두면 HPA가 정상 동작했는데도 k6가 "실패"로 표시해 데모 중 오해를 살 수 있다.
    // 결과는 k6 요약이 아니라 Grafana(README "결과 확인" 참고)로 판단한다.
    //
    // CI에서 자동 판정이 필요하면 아래 주석을 해제:
    // thresholds: {
    //     http_req_failed: ['rate<0.05'],
    // },
};

export function setup() {
    // 하드코딩된 종목 대신, 실제 적재된 마켓 목록을 받아 부하에 사용한다.
    // Collector가 데이터를 안 넣은 환경이면 여기서 즉시 에러로 알려준다(헛부하 방지).
    const res = http.get(`${BASE_URL}/api/v1/markets`);
    if (res.status !== 200) {
        throw new Error(`마켓 목록 조회 실패 (status=${res.status}). BASE_URL을 확인하세요: ${BASE_URL}`);
    }

    const body = JSON.parse(res.body);
    const markets = (body.data || []).filter((m) => m.exchange && m.market);
    if (markets.length === 0) {
        throw new Error('마켓 목록이 비어있습니다. Collector가 데이터를 적재했는지 확인하세요.');
    }
    console.log(`setup 완료 — 마켓 ${markets.length}개로 부하 시작`);
    return { markets };
}

function pick(arr) {
    return arr[Math.floor(Math.random() * arr.length)];
}

export default function (data) {
    const market = pick(data.markets);
    const scenario = Math.random();

    if (scenario < 0.4) {
        // 40%: 관심목록 전체 시세 조회
        const exchange = pick(EXCHANGES);
        const res = http.get(`${BASE_URL}/api/v1/tickers?exchange=${exchange}&category=ALL&limit=100`, {
            tags: { name: 'tickers_list' },
            responseType: 'none', // status만 확인 → 바디는 버려서 생성기 메모리/CPU 절약
        });
        if (!check(res, { 'tickers list 200': (r) => r.status === 200 })) {
            tickerErrors.add(1);
        }
    } else if (scenario < 0.7) {
        // 30%: 단일 종목 현재가(대시보드 자동 갱신 흉내)
        const res = http.get(`${BASE_URL}/api/v1/tickers/${market.exchange}/${market.market}`, {
            tags: { name: 'ticker_single' },
            responseType: 'none',
        });
        if (!check(res, { 'ticker single 200': (r) => r.status === 200 })) {
            tickerErrors.add(1);
        }
    } else {
        // 30%: 캔들(차트) 조회 — 1s(Redis)·1m(MySQL)을 섞어 백엔드 부하도 함께 관측
        const interval = pick(INTERVALS);
        const res = http.get(
            `${BASE_URL}/api/v1/candles?exchange=${market.exchange}&market=${market.market}&interval=${interval}&limit=200`,
            { tags: { name: `candles_${interval}` }, responseType: 'none' }
        );
        if (!check(res, { 'candles 200': (r) => r.status === 200 })) {
            candleErrors.add(1);
        }
    }

    sleep(Math.random() * 0.5 + 0.1); // 100~600ms 간격, 실사용자 패턴 흉내
}
