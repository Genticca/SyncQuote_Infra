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
//   주기마다:
//     - candles:1s:stream → consumer group으로 소비(ack-then-trim) → 1분봉 적재
//       (적재 확인 전엔 ack 안 함 → 유실 방지. List는 API 실시간용으로 분리)
//     - ticker:*:*        → 각 Stream의 최신 1건을 ticker_latest 로
//     - markets:*         → 각 Hash를 market 테이블로
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
	markets := a.archiveMarkets(ctx)       // 1) FK 부모 먼저
	candles := a.archiveCandlesStream(ctx) // 2) 캔들 (Stream consumer group)
	tickers := a.archiveTickers(ctx)       // 3) 현재가
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

// 캔들 영속화용 Stream consumer group 설정.
const (
	candleGroup    = "archiver"   // consumer group 이름
	candleConsumer = "archiver-1" // consumer 이름(고정 → 재시작해도 같은 PEL 재개)
	candlePageSize = 2000         // XReadGroup 1회 페이지 크기
)

// EnsureCandleGroup : 캔들 Stream과 consumer group을 보장한다(없으면 생성).
// "0"부터 시작해 기존 백로그도 빠짐없이 소비한다. main이 아카이버 기동 전 1회 호출.
func EnsureCandleGroup(ctx context.Context, rdb *redis.Client) {
	err := rdb.XGroupCreateMkStream(ctx, CandleStreamKey(), candleGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		log.Printf("[archive] consumer group 생성 실패: %v", err)
	}
}

// streamCandle : Stream 항목 1건 (ID + 원본 JSON + 파싱된 1초봉).
type streamCandle struct {
	id  string
	raw string
	c   SecondCandle
}

// archiveCandlesStream : 캔들 Stream을 consumer group으로 소비해 "완성된 분"을
// 1분봉으로 집계·MySQL 적재하고, 적재에 성공한 분의 항목만 XACK + XDEL 한다.
//
// [정합성 핵심]
//   - 항목은 ack 전까지 PEL(대기목록)에 남는다 → MySQL 적재 성공 후에만 ack.
//   - 적재 실패/크래시 → 미ack → 다음 사이클 재처리(at-least-once).
//   - candle PK UPSERT라 재처리해도 중복 없음(사실상 정확히 한 번).
//   - 진행 중인 분(미완성)의 항목은 ack하지 않아 다음 사이클까지 보존된다.
func (a *Archiver) archiveCandlesStream(ctx context.Context) int {
	entries := a.readCandleEntries(ctx)
	if len(entries) == 0 {
		return 0
	}
	// 현재 분의 시작 시각(Unix). 이 시각 이상인 1초봉은 "진행 중인 분"이라 제외.
	currentMinuteStart := time.Now().Unix() / 60 * 60

	// 종목별로 묶는다. (aggregateMinuteCandles는 단일 종목 입력을 전제하므로)
	type bucket struct {
		jsons       []string
		idsByMinute map[int64][]string
	}
	buckets := make(map[string]*bucket)
	for _, e := range entries {
		sym := e.c.Exchange + ":" + e.c.Code
		b := buckets[sym]
		if b == nil {
			b = &bucket{idsByMinute: make(map[int64][]string)}
			buckets[sym] = b
		}
		b.jsons = append(b.jsons, e.raw)
		m := e.c.Time / 60 * 60
		b.idsByMinute[m] = append(b.idsByMinute[m], e.id)
	}

	count := 0
	var ackDel []string
	for _, b := range buckets {
		// 완성된 분만 1분봉으로 집계 (순수 함수 재사용).
		for _, candle := range aggregateMinuteCandles(b.jsons, currentMinuteStart) {
			if err := SaveCandleToMySQL(ctx, a.db, candle, "1m"); err != nil {
				log.Println("[archive] 1분봉 저장 실패:", err)
				continue // ack 안 함 → 다음 사이클 재처리
			}
			count++
			// 이 완성분을 구성한 1초봉 항목들만 ack/del 대상에 추가.
			ackDel = append(ackDel, b.idsByMinute[candle.Time]...)
		}
	}

	// 적재 확인된 항목만 ack(PEL 제거) 후 del(메모리 회수). 미완성 분 항목은 보존.
	if len(ackDel) > 0 {
		if err := a.rdb.XAck(ctx, CandleStreamKey(), candleGroup, ackDel...).Err(); err != nil {
			log.Printf("[archive] XACK 실패: %v", err)
		}
		if err := a.rdb.XDel(ctx, CandleStreamKey(), ackDel...).Err(); err != nil {
			log.Printf("[archive] XDEL 실패: %v", err)
		}
	}
	return count
}

// readCandleEntries : consumer의 미ack 항목(PEL)과 새 항목(">")을 모두 읽어 파싱한다.
// PEL을 먼저 읽어 "지난 사이클의 미완성 분 + 재시작 후 복구분"을 재처리한다.
func (a *Archiver) readCandleEntries(ctx context.Context) []streamCandle {
	var out []streamCandle

	collect := func(streams []redis.XStream) (n int, lastID string) {
		for _, st := range streams {
			for _, msg := range st.Messages {
				n++
				lastID = msg.ID
				raw, _ := msg.Values["json"].(string)
				if raw == "" {
					continue
				}
				var c SecondCandle
				if json.Unmarshal([]byte(raw), &c) != nil {
					continue
				}
				out = append(out, streamCandle{id: msg.ID, raw: raw, c: c})
			}
		}
		return n, lastID
	}

	read := func(from string) ([]redis.XStream, error) {
		return a.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    candleGroup,
			Consumer: candleConsumer,
			Streams:  []string{CandleStreamKey(), from},
			Count:    candlePageSize,
			Block:    -1, // 논블로킹 (데이터 없으면 즉시 반환)
		}).Result()
	}

	// 1) 미ack 항목(PEL) — 커서로 페이지네이션 ("0"부터, 마지막 ID로 진행)
	cursor := "0"
	for {
		streams, err := read(cursor)
		if err != nil {
			if err != redis.Nil {
				log.Printf("[archive] XReadGroup(PEL) 실패: %v", err)
			}
			break
		}
		n, lastID := collect(streams)
		if n < candlePageSize || lastID == "" {
			break
		}
		cursor = lastID
	}

	// 2) 새 항목(">") — 소비될수록 자동 진행하므로 반복 호출로 드레인
	for {
		streams, err := read(">")
		if err != nil {
			if err != redis.Nil {
				log.Printf("[archive] XReadGroup(new) 실패: %v", err)
			}
			break
		}
		n, _ := collect(streams)
		if n < candlePageSize {
			break
		}
	}

	return out
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
