// ============================================================================
// common/persist.go — Redis → MySQL 아카이빙 배치
// ----------------------------------------------------------------------------
// 팀원 코드의 "영속화" 아이디어를 우리 구조에 맞게 구현한다.
//
// [왜 큐가 아니라 주기 배치인가]
//   데이터 1건마다 영속화 큐(ZSet+Hash)에 넣었다. 하지만 우리
//   수집기는 1초봉이 종목당 초당 1개씩(수백 종목 × 2거래소) 쏟아져, 매 건을
//   큐에 넣으면 큐가 폭발한다. 대신 "1분마다 Redis를 스캔해 MySQL로 옮기는
//   배치"가 우리 구조에 맞는다. (우리가 처음부터 'TTL 90초 > 배치주기 60초'로
//   설계한 그 배치)
//
// [동작]
//   60초마다:
//     - candles:1s:*:*  → 각 List를 읽어 candle 테이블로 (exchange 구분 유지)
//     - ticker:*:*      → 각 Stream의 최신 1건을 ticker_snapshot 으로
//     - markets:*       → 각 Hash를 market 테이블로
//   TTL(90초)이 배치주기(60초)보다 길어, 옮기기 전에 사라지지 않는다.
//
// [거래소 구분]
//   키가 candles:1s:upbit:KRW-BTC 처럼 exchange를 품고 있으므로, 키를 파싱해
//   exchange를 꺼내 그대로 테이블 컬럼에 넣는다. → 한 테이블에 두 거래소가
//   행으로 분리돼 쌓인다.
// ============================================================================

package common

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Archiver : Redis를 주기적으로 스캔해 MySQL로 옮기는 배치.
type Archiver struct {
	rdb *redis.Client
	db  *sql.DB
}

func NewArchiver(rdb *redis.Client, db *sql.DB) *Archiver {
	return &Archiver{rdb: rdb, db: db}
}

// Run : interval 마다 아카이빙을 수행. main이 goroutine으로 띄운다.
func (a *Archiver) Run(ctx context.Context, interval time.Duration) {
	if a.db == nil {
		log.Println("[archive] MySQL 미연결 — 아카이빙 비활성")
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("[archive] 시작 (주기 %s)\n", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.archiveOnce(ctx)
		}
	}
}

// archiveOnce : 1회 아카이빙.
// [순서 중요] 페어(market_info) → 캔들 → 현재가 순으로 저장한다.
// candles/ticker_latest 가 market_info 를 FK로 참조하므로, 페어가 먼저
// 있어야 캔들·현재가 INSERT 시 FK 위반이 안 난다.
func (a *Archiver) archiveOnce(ctx context.Context) {
	markets := a.archiveMarkets(ctx) // 1) FK 부모 먼저
	candles := a.archiveCandles(ctx) // 2) 캔들
	tickers := a.archiveTickers(ctx) // 3) 현재가
	log.Printf("[archive] 완료: 페어 %d, 캔들 %d, 현재가 %d\n", markets, candles, tickers)
}

// scanKeys : 패턴에 맞는 키를 SCAN으로 모은다. (KEYS는 운영서 위험해서 SCAN 사용)
func (a *Archiver) scanKeys(ctx context.Context, pattern string) []string {
	var keys []string
	var cursor uint64
	for {
		batch, next, err := a.rdb.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			log.Println("[archive] SCAN 실패:", err)
			break
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return keys
}

// archiveCandles : candles:1s:{exchange}:{code} List(1초봉)를 읽어
// "1분봉으로 집계"한 뒤 candle 테이블에 candle_unit='1m'으로 저장한다.
//
// [왜 1분봉으로 묶나]
//
//	1초봉을 그대로 저장하면 종목 수백 개 × 초당 1개라 데이터가 폭증한다.
//	60개의 1초봉을 1분봉 1개로 합치면 데이터가 60배 줄고, 과거 차트용으로도
//	1분봉이 표준이다. (실시간 1초 디테일은 Redis가 담당)
//
// [완성된 분만 저장]
//
//	현재 진행 중인 분(아직 60초가 안 지난 분)은 미완성이라 저장하지 않는다.
//	현재 시각의 "분"보다 이전 분들만 확정된 것으로 보고 저장한다.
func (a *Archiver) archiveCandles(ctx context.Context) int {
	count := 0
	// 현재 분의 시작 시각(Unix). 이 시각 이상인 1초봉은 "아직 진행 중인 분"이라 제외.
	currentMinuteStart := time.Now().Unix() / 60 * 60

	for _, key := range a.scanKeys(ctx, "candles:1s:*:*") {
		items, err := a.rdb.LRange(ctx, key, 0, -1).Result()
		if err != nil {
			log.Printf("[archive] LRange 실패 key=%s: %v", key, err)
			continue
		}

		// [역할 분리] 1초봉 → 1분봉 "집계(연산)"는 순수 함수에 위임하고,
		// 여기서는 "읽기(Redis) → 저장(MySQL)"이라는 I/O만 담당한다.
		// 이렇게 분리해 두면 집계 로직을 독립적으로 테스트할 수 있고, 나중에
		// 별도 Calculator 프로세스로 떼어낼 때 이 함수만 그대로 옮기면 된다.
		candles := aggregateMinuteCandles(items, currentMinuteStart)

		for i := range candles {
			if err := SaveCandleToMySQL(ctx, a.db, candles[i], "1m"); err != nil {
				log.Println("[archive] 1분봉 저장 실패:", err)
				continue
			}
			count++
		}
	}
	return count
}

// aggregateMinuteCandles : 1초봉 JSON 문자열들을 받아 "완성된 분"의 1분봉으로
// 집계한다. Redis/MySQL을 전혀 모르는 순수 함수 — 입력(1초봉 raw 슬라이스)과
// 기준 시각만 주면 출력(1분봉 슬라이스)이 결정된다. (단위 테스트가 쉽다)
//
// [완성된 분만 저장] currentMinuteStart 이상인 1초봉은 아직 진행 중인 분이라
// 제외한다. 그 분의 1초봉이 더 들어올 수 있어 지금 저장하면 미완성 봉이 된다.
//
// [시가/종가는 시각으로 결정] List가 보통 시간순이지만 재연결 등으로 순서가
// 뒤섞일 수 있어, 원소 순서가 아니라 1초봉의 Time(초 시작)을 비교해 그 분에서
// 가장 이른 봉의 시가 / 가장 늦은 봉의 종가를 채택한다.
func aggregateMinuteCandles(items []string, currentMinuteStart int64) []SecondCandle {
	// 분키(분 시작 Unix) → 집계 중인 1분봉
	minutes := make(map[int64]*SecondCandle)
	minOpenTime := make(map[int64]int64)  // 분키 → 그 분에서 가장 이른 1초봉 시각
	maxCloseTime := make(map[int64]int64) // 분키 → 그 분에서 가장 늦은 1초봉 시각

	for _, raw := range items {
		var c SecondCandle
		if json.Unmarshal([]byte(raw), &c) != nil {
			continue
		}
		minuteStart := c.Time / 60 * 60 // 이 1초봉이 속한 분의 시작 시각

		// 아직 진행 중인 분은 건너뛴다 (완성된 분만 저장).
		if minuteStart >= currentMinuteStart {
			continue
		}

		agg, exists := minutes[minuteStart]
		if !exists {
			// 그 분의 첫 1초봉 → 1분봉 초기화.
			minutes[minuteStart] = &SecondCandle{
				Exchange: c.Exchange,
				Code:     c.Code,
				Time:     minuteStart, // 1분봉의 시각 = 분 시작
				Open:     c.Open,
				High:     c.High,
				Low:      c.Low,
				Close:    c.Close,
				Volume:   c.Volume,
			}
			minOpenTime[minuteStart] = c.Time
			maxCloseTime[minuteStart] = c.Time
			continue
		}

		// 고가/저가 갱신, 거래량 누적.
		if c.High > agg.High {
			agg.High = c.High
		}
		if c.Low < agg.Low {
			agg.Low = c.Low
		}
		agg.Volume += c.Volume

		// 시가/종가는 "시각"으로 결정 (순서 의존 제거).
		if c.Time < minOpenTime[minuteStart] {
			minOpenTime[minuteStart] = c.Time
			agg.Open = c.Open
		}
		if c.Time > maxCloseTime[minuteStart] {
			maxCloseTime[minuteStart] = c.Time
			agg.Close = c.Close
		}
	}

	// map → 슬라이스로 변환해 반환. (저장은 호출부가 담당)
	out := make([]SecondCandle, 0, len(minutes))
	for _, agg := range minutes {
		out = append(out, *agg)
	}
	return out
}

// archiveTickers : ticker:{exchange}:{code} Stream 최신 1건을 ticker_snapshot 으로.
func (a *Archiver) archiveTickers(ctx context.Context) int {
	count := 0
	for _, key := range a.scanKeys(ctx, "ticker:*:*") {
		// ticker:rest:... (52주 메타 Hash)는 제외 — 스냅샷 대상 아님.
		if strings.HasPrefix(key, "ticker:rest:") {
			continue
		}
		// 키에서 exchange/code 추출: ticker:{exchange}:{code}
		// code 안에 ':'가 없다고 가정(업비트 KRW-BTC, 바이낸스 BTCUSDT 모두 안전).
		parts := strings.SplitN(key, ":", 3)
		if len(parts) != 3 {
			continue
		}
		exch, code := parts[1], parts[2]

		// Stream 최신 1건.
		msgs, err := a.rdb.XRevRangeN(ctx, key, "+", "-", 1).Result()
		if err != nil || len(msgs) == 0 {
			continue
		}
		t := tickerFromStream(msgs[0].Values)
		// Stream 값에 exchange가 있지만, code는 안 넣었으므로 키에서 보강.
		t.Exchange = exch
		t.Code = code
		// change_status는 NOT NULL 컬럼이다. 혹시 빈 값이면 기본값 EVEN으로.
		if t.Change == "" {
			t.Change = "EVEN"
		}

		if err := SaveTickerToMySQL(ctx, a.db, t); err != nil {
			log.Println("[archive] 현재가 저장 실패:", err)
			continue
		}
		count++
	}
	return count
}

// archiveMarkets : markets:{exchange} Hash 를 market 테이블로.
func (a *Archiver) archiveMarkets(ctx context.Context) int {
	count := 0
	for _, key := range a.scanKeys(ctx, "markets:*") {
		all, err := a.rdb.HGetAll(ctx, key).Result()
		if err != nil {
			continue
		}
		for _, raw := range all {
			var m MarketInfo
			if json.Unmarshal([]byte(raw), &m) != nil {
				continue
			}
			if err := SaveMarketToMySQL(ctx, a.db, m); err != nil {
				continue
			}
			count++
		}
	}
	return count
}

// tickerFromStream : Stream 엔트리(map[string]interface{})를 TickerData로 복원.
// Exchange/Code는 호출부에서 키 기반으로 보강하므로 여기선 채우지 않는다.
func tickerFromStream(v map[string]interface{}) TickerData {
	get := func(k string) string {
		if val, ok := v[k]; ok {
			if s, ok := val.(string); ok {
				return s
			}
		}
		return ""
	}
	atof := func(k string) float64 { return parseFloatStr(get(k)) }
	atoi := func(k string) int64 { return parseIntStr(get(k)) }

	return TickerData{
		TradePrice:        atof("trade_price"),
		Change:            get("change"),
		ChangeRate:        atof("change_rate"),
		SignedChangeRate:  atof("signed_change_rate"),
		SignedChangePrice: atof("signed_change_price"),
		AccTradePrice24h:  atof("acc_trade_price_24h"),
		AccTradeVolume24h: atof("acc_trade_volume_24h"),
		HighPrice:         atof("high_price"),
		LowPrice:          atof("low_price"),
		Timestamp:         atoi("timestamp"),
	}
}

// parseFloatStr / parseIntStr : 문자열 → 숫자. 실패 시 0.
func parseFloatStr(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseIntStr(s string) int64 {
	i, _ := strconv.ParseInt(s, 10, 64)
	return i
}
