// ============================================================================
// common/writer.go — Redis 저장 로직 (모든 거래소 공통)
// ----------------------------------------------------------------------------
// 어댑터가 공통 구조(SecondCandle / TickerData)로 변환해 넘기면, 여기서
// 거래소 구분 없이 Redis에 적재한다. "어느 거래소냐"는 구조체의 Exchange
// 필드에 이미 들어있으므로, 이 코드는 그 값을 키 조립에만 쓰면 된다.
//
// 통합의 효과: 업비트용/바이낸스용 저장 코드가 따로 있던 걸 이 한 벌로 합쳤다.
// ============================================================================

package common

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// DataTTL : 1초봉·현재가 공통 TTL. (1분 30초)
// 다음 단계 "1분 주기 MySQL 배치"가 읽으러 올 때까지 살아있도록 60초보다 길게.
const DataTTL = 90 * time.Second

// candleMaxLen : 1초봉 List의 개수 상한(방어선).
// 정상 동작 시 TTL 90초면 1초봉은 종목당 최대 ~90개라 이 상한에 닿지 않는다.
// 다만 시계 꼬임·TTL 미적용 같은 비정상 상황에서 무한 증가를 막는 안전장치.
// (TTL + 개수상한 이중 안전장치. 정상엔 영향 없고, 비정상엔 상한 보장)
const candleMaxLen = 120

// Writer : Redis 클라이언트를 감싼 공통 저장기.
type Writer struct {
	rdb *redis.Client
}

// NewWriter : 저장기 생성.
func NewWriter(rdb *redis.Client) *Writer {
	return &Writer{rdb: rdb}
}

// SaveCandle : 확정된 1초봉을 List에 적재.
// TTL(시간 기준) + LTrim(개수 기준) 이중 안전장치로 청소한다.
// 업비트(집계로 만든 봉)·바이낸스(거래소가 준 봉) 모두 이 함수로 저장된다.
func (w *Writer) SaveCandle(ctx context.Context, c SecondCandle) {
	key := SecondCandleKey(c.Exchange, c.Code)
	data, err := json.Marshal(c)
	if err != nil {
		// 구조체 → JSON 변환 실패. 이 봉은 적재 불가이므로 버리되, 조용히
		// 사라지지 않도록 반드시 남긴다. (이전: 에러를 _로 무시했음)
		log.Printf("[writer] 캔들 marshal 실패 key=%s: %v", key, err)
		return
	}

	pipe := w.rdb.Pipeline()
	// (A) API 실시간 차트용 List — TTL+개수상한. 유실돼도 무해(캐시).
	pipe.RPush(ctx, key, data)
	// 최근 candleMaxLen개만 유지 (-N ~ -1 = 뒤에서 N개). 비정상 증가 방어선.
	pipe.LTrim(ctx, key, -candleMaxLen, -1)
	pipe.Expire(ctx, key, DataTTL)
	// (B) MySQL 적재 보장용 Stream — TTL 없음. 아카이버가 ack할 때까지 보존된다.
	//     (정합성 핵심: 적재 확인 전엔 절대 안 지움)
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: CandleStreamKey(),
		Values: map[string]interface{}{"json": string(data)},
	})
	// Pipeline 실행 실패 = Redis 적재 실패 = 데이터 유실. 반드시 로깅한다.
	// (noeviction이면 메모리 가득 시 여기서 에러 → 조용한 유실 대신 가시화)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("[writer] 캔들 적재 실패 key=%s: %v", key, err)
	}
}

// SaveTicker : 현재가를 Stream에 적재. 필드 이름은 두 거래소 통일 규격.
func (w *Writer) SaveTicker(ctx context.Context, t TickerData) {
	key := TickerKey(t.Exchange, t.Code)

	pipe := w.rdb.Pipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		Values: map[string]interface{}{
			"exchange":             t.Exchange,
			"trade_price":          t.TradePrice,
			"change":               t.Change,
			"change_rate":          t.ChangeRate,       // 절댓값 (통일)
			"signed_change_rate":   t.SignedChangeRate, // 부호 있음
			"signed_change_price":  t.SignedChangePrice,
			"acc_trade_price_24h":  t.AccTradePrice24h,
			"acc_trade_volume_24h": t.AccTradeVolume24h,
			"high_price":           t.HighPrice,
			"low_price":            t.LowPrice,
			"timestamp":            t.Timestamp,
		},
	})
	pipe.Expire(ctx, key, DataTTL)
	// 현재가 적재 실패도 로깅한다. (이전: 반환값 무시)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("[writer] 현재가 적재 실패 key=%s: %v", key, err)
	}
}

// SaveMarket : 페어 정보를 Hash에 저장. (markets:{exchange})
func (w *Writer) SaveMarket(ctx context.Context, m MarketInfo) {
	data, err := json.Marshal(m)
	if err != nil {
		log.Printf("[writer] 페어 marshal 실패 %s/%s: %v", m.Exchange, m.Market, err)
		return
	}
	if err := w.rdb.HSet(ctx, MarketsKey(m.Exchange), m.Market, data).Err(); err != nil {
		log.Printf("[writer] 페어 적재 실패 %s/%s: %v", m.Exchange, m.Market, err)
	}
}

// SaveMarketsBatch : 페어 여러 건을 파이프라인으로 한 번에 저장.
func (w *Writer) SaveMarketsBatch(ctx context.Context, markets []MarketInfo) error {
	pipe := w.rdb.Pipeline()
	for _, m := range markets {
		data, err := json.Marshal(m)
		if err != nil {
			// 한 건이 깨져도 나머지는 저장되도록 건너뛰고 로깅만 한다.
			log.Printf("[writer] 페어 marshal 실패(배치) %s/%s: %v", m.Exchange, m.Market, err)
			continue
		}
		pipe.HSet(ctx, MarketsKey(m.Exchange), m.Market, data)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// SaveRestTicker : 52주 메타 등 REST 전용 값을 Hash로 저장. (업비트에서 사용)
func (w *Writer) SaveRestTicker(ctx context.Context, exchange, code string, fields map[string]interface{}) {
	if err := w.rdb.HSet(ctx, RestTickerKey(exchange, code), fields).Err(); err != nil {
		log.Printf("[writer] REST ticker 적재 실패 %s/%s: %v", exchange, code, err)
	}
}
