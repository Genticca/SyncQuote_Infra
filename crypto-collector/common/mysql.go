// ============================================================================
// common/mysql.go — MySQL 연결 + 영구 저장 로직
// ----------------------------------------------------------------------------
// 테이블은 팀원 SQL로 "수동 생성"하므로, 이 코드는 테이블을 만들지 않고
// INSERT/UPSERT만 한다. (EnsureSchema 없음)
//
// 스키마 매핑:
//   market_info    : 페어 (PK: exchange, market)
//   candles        : 봉 (PK: exchange, market, interval_type, candle_time[BIGINT])
//   ticker_latest  : 현재가 최신 (PK: exchange, market) — 이력(realtime)은 미사용
//
// FK: candles/ticker_latest → market_info. 따라서 아카이빙은 "페어 먼저"
//     저장해야 FK 위반이 안 난다. (순서는 persist.go에서 보장)
// ============================================================================

package common

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ConnectMySQL : DSN으로 MySQL 연결. 5회까지 ping 재시도.
// (MySQL 컨테이너는 기동 직후 바로 연결을 못 받으므로 재시도가 필요)
func ConnectMySQL(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql open error: %w", err)
	}
	for attempt := 1; attempt <= 5; attempt++ {
		if err := db.PingContext(ctx); err == nil {
			return db, nil
		}
		if attempt == 5 {
			return db, fmt.Errorf("mysql ping failed after 5 attempts")
		}
		log.Printf("mysql ping %d/5 실패, 재시도", attempt)
		time.Sleep(2 * time.Second)
	}
	return db, nil
}

// --- 저장 함수들 (UPSERT) ---
//
// [왜 INSERT ... ON DUPLICATE KEY UPDATE (UPSERT)인가]
//   아카이빙 배치가 60초마다 돌면서 같은 캔들/페어/현재가를 다시 넣을 수 있다.
//   UPSERT를 쓰면 PK가 같은 행은 중복 INSERT 대신 UPDATE로 덮어써져,
//   중복 행이 안 생긴다. (PK에 exchange가 포함돼 거래소가 섞이지 않는다)

// SaveMarketToMySQL : 페어 정보를 market_info 에 upsert.
// FK 대상이므로 캔들·현재가보다 "먼저" 저장돼야 한다.
func SaveMarketToMySQL(ctx context.Context, db *sql.DB, m MarketInfo) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO market_info
			(exchange, market, korean_name, english_name, market_warning, event_warning, event_caution)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			korean_name = VALUES(korean_name),
			english_name = VALUES(english_name),
			market_warning = VALUES(market_warning),
			event_warning = VALUES(event_warning),
			event_caution = VALUES(event_caution)
	`, m.Exchange, m.Market, m.KoreanName, m.EnglishName, m.MarketWarning, m.EventWarning, m.EventCaution)
	return err
}

// SaveCandleToMySQL : 캔들 1건을 candles 에 upsert.
// candle_time은 팀원 스키마가 BIGINT(Unix seconds)이므로 c.Time을 그대로 넣는다.
// interval은 봉 단위("1m" 등). 컬럼명은 팀원 스키마(open_price/close_price/volume)를 따른다.
func SaveCandleToMySQL(ctx context.Context, db *sql.DB, c SecondCandle, interval string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO candles
			(exchange, market, interval_type, candle_time,
			 open_price, high_price, low_price, close_price, volume)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			open_price = VALUES(open_price),
			high_price = VALUES(high_price),
			low_price = VALUES(low_price),
			close_price = VALUES(close_price),
			volume = VALUES(volume)
	`, c.Exchange, c.Code, interval, c.Time,
		c.Open, c.High, c.Low, c.Close, c.Volume)
	return err
}

// SaveTickerToMySQL : 현재가를 ticker_latest 에 upsert (종목당 1행, 최신값 덮어쓰기).
// 이력 테이블(ticker_realtime)은 이번 설계에서 사용하지 않는다.
func SaveTickerToMySQL(ctx context.Context, db *sql.DB, t TickerData) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO ticker_latest
			(exchange, market, trade_price, change_status, change_rate,
			 signed_change_rate, signed_change_price,
			 acc_trade_price_24h, acc_trade_volume_24h,
			 high_price, low_price, event_timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			trade_price = VALUES(trade_price),
			change_status = VALUES(change_status),
			change_rate = VALUES(change_rate),
			signed_change_rate = VALUES(signed_change_rate),
			signed_change_price = VALUES(signed_change_price),
			acc_trade_price_24h = VALUES(acc_trade_price_24h),
			acc_trade_volume_24h = VALUES(acc_trade_volume_24h),
			high_price = VALUES(high_price),
			low_price = VALUES(low_price),
			event_timestamp = VALUES(event_timestamp)
	`, t.Exchange, t.Code, t.TradePrice, t.Change, t.ChangeRate,
		t.SignedChangeRate, t.SignedChangePrice,
		t.AccTradePrice24h, t.AccTradeVolume24h,
		t.HighPrice, t.LowPrice, t.Timestamp)
	return err
}
