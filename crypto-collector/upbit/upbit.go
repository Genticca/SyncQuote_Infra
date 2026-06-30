// ============================================================================
// upbit/upbit.go — 업비트 어댑터
// ----------------------------------------------------------------------------
// 역할: 업비트의 "원본 JSON"을 파싱해서 common 패키지의 "공통 구조"로 변환한다.
//   - 페어 목록(REST)      → common.MarketInfo
//   - 체결(WebSocket trade) → common.Trade  → 집계기 → 1초봉
//   - 현재가(WebSocket ticker) → common.TickerData
//   - 52주 메타(REST ticker) → Hash 직접 저장
//
// 업비트 고유 특징:
//   - 1초봉을 안 줌 → 체결을 모아 common.Aggregator로 직접 집계.
//   - 가격이 숫자(float)로 옴.
//   - 등락률 change_rate가 이미 절댓값.
// ============================================================================

package upbit

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"crypto-collector/common"
)

// 이 어댑터의 거래소 식별자. 공통부에 이 값을 넘겨 키/구조에 박는다.
const exchange = "upbit"

const (
	marketAllURL      = "https://api.upbit.com/v1/market/all?is_details=true"
	restTickerURL     = "https://api.upbit.com/v1/ticker"
	wsURL             = "wss://api.upbit.com/websocket/v1"
	restTickerEvery   = 1 * time.Minute
	restTickerBatch   = 100
)

// --- 업비트 원본 JSON 구조체 (파싱 전용) ---

type upbitMarket struct {
	Market        string `json:"market"`
	KoreanName    string `json:"korean_name"`
	EnglishName   string `json:"english_name"`
	MarketWarning string `json:"market_warning"`
	MarketEvent   struct {
		Warning bool `json:"warning"`
		Caution struct {
			PriceFluctuations            bool `json:"PRICE_FLUCTUATIONS"`
			TradingVolumeSoaring         bool `json:"TRADING_VOLUME_SOARING"`
			DepositAmountSoaring         bool `json:"DEPOSIT_AMOUNT_SOARING"`
			GlobalPriceDifferences       bool `json:"GLOBAL_PRICE_DIFFERENCES"`
			ConcentrationOfSmallAccounts bool `json:"CONCENTRATION_OF_SMALL_ACCOUNTS"`
		} `json:"caution"`
	} `json:"market_event"`
}

type upbitTrade struct {
	Type        string  `json:"type"`
	Code        string  `json:"code"`
	TradePrice  float64 `json:"trade_price"`
	TradeVolume float64 `json:"trade_volume"`
	Timestamp   int64   `json:"timestamp"`
}

type upbitTicker struct {
	Type              string  `json:"type"`
	Code              string  `json:"code"`
	TradePrice        float64 `json:"trade_price"`
	Change            string  `json:"change"`
	ChangeRate        float64 `json:"change_rate"`         // 이미 절댓값
	SignedChangeRate  float64 `json:"signed_change_rate"`
	SignedChangePrice float64 `json:"signed_change_price"`
	AccTradePrice24h  float64 `json:"acc_trade_price_24h"`
	AccTradeVolume24h float64 `json:"acc_trade_volume_24h"`
	HighPrice         float64 `json:"high_price"`
	LowPrice          float64 `json:"low_price"`
	Timestamp         int64   `json:"timestamp"`
}

type upbitRestTicker struct {
	Market             string  `json:"market"`
	TradePrice         float64 `json:"trade_price"`
	PrevClosingPrice   float64 `json:"prev_closing_price"`
	Highest52WeekPrice float64 `json:"highest_52_week_price"`
	Highest52WeekDate  string  `json:"highest_52_week_date"`
	Lowest52WeekPrice  float64 `json:"lowest_52_week_price"`
	Lowest52WeekDate   string  `json:"lowest_52_week_date"`
}

func fetch(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Run : 업비트 수집 전체를 실행. main이 goroutine으로 호출한다.
// wg에 내부 goroutine들을 등록해, main이 종료 시 완료를 기다릴 수 있게 한다.
// 1) 페어 로드(동기) → codes 확보  2) 집계기·웹소켓·REST폴링 병렬 실행.
func Run(ctx context.Context, w *common.Writer, wg *sync.WaitGroup) {
	codes, err := loadMarkets(ctx, w)
	if err != nil {
		log.Println("[upbit] 페어 로드 실패:", err)
		return
	}
	log.Printf("[upbit] 페어 %d개 로드\n", len(codes))

	// 1초봉 집계기 (업비트는 체결로 직접 집계).
	agg := common.NewAggregator(w)

	// 내부 goroutine 3개를 각각 wg에 등록. 종료 시 main이 모두 기다린다.
	wg.Add(3)
	go func() { defer wg.Done(); agg.RunFlushLoop(ctx) }()
	go func() { defer wg.Done(); runWebSocket(ctx, w, agg, codes) }()
	go func() { defer wg.Done(); runRestTickerPoller(ctx, w, codes) }()
}

// loadMarkets : 페어 목록을 공통 MarketInfo로 변환해 저장하고 코드 목록 반환.
func loadMarkets(ctx context.Context, w *common.Writer) ([]string, error) {
	body, err := fetch(marketAllURL)
	if err != nil {
		return nil, err
	}
	var markets []upbitMarket
	if err := json.Unmarshal(body, &markets); err != nil {
		return nil, err
	}

	var codes []string
	var infos []common.MarketInfo
	for _, m := range markets {
		if !strings.HasPrefix(m.Market, "KRW-") {
			continue
		}
		c := m.MarketEvent.Caution
		infos = append(infos, common.MarketInfo{
			Exchange:      exchange,
			Market:        m.Market,
			KoreanName:    m.KoreanName,
			EnglishName:   m.EnglishName,
			MarketWarning: m.MarketWarning,
			EventWarning:  m.MarketEvent.Warning,
			EventCaution: c.PriceFluctuations || c.TradingVolumeSoaring ||
				c.DepositAmountSoaring || c.GlobalPriceDifferences ||
				c.ConcentrationOfSmallAccounts,
		})
		codes = append(codes, m.Market)
	}
	if err := w.SaveMarketsBatch(ctx, infos); err != nil {
		return nil, err
	}
	return codes, nil
}

// runWebSocket : 한 연결로 trade+ticker 구독. 끊기면 재연결.
func runWebSocket(ctx context.Context, w *common.Writer, agg *common.Aggregator, codes []string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			log.Println("[upbit] WS 연결 실패, 3초 후 재시도:", err)
			time.Sleep(3 * time.Second)
			continue
		}
		log.Println("[upbit] WS 연결 성공")

		sub := []interface{}{
			map[string]string{"ticket": uuid.NewString()},
			map[string]interface{}{"type": "trade", "codes": codes},
			map[string]interface{}{"type": "ticker", "codes": codes},
		}
		if err := conn.WriteJSON(sub); err != nil {
			conn.Close()
			continue
		}

		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			default:
			}

			// 무수신 60초면 죽은 연결로 간주(업비트는 수시로 데이터가 옴). ctx 종료는
			// 위 select 에서 처리한다.
			// [버그수정] 이전엔 1초 deadline 후 timeout이면 continue(같은 연결 재read)였는데,
			// deadline이 프레임 수신 도중 발동하면 gorilla 연결의 읽기 상태가 깨져 이후
			// ReadMessage가 "repeated read on failed websocket connection" panic을 내
			// 프로세스가 죽었다. → timeout이든 끊김이든 break하여 같은 연결에 재read하지 않고 재연결.
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Println("[upbit] 연결 끊김/무수신, 재연결:", err)
				break
			}

			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(msg, &probe) != nil {
				continue
			}

			switch probe.Type {
			case "trade":
				var t upbitTrade
				if json.Unmarshal(msg, &t) == nil {
					// 업비트 원본 → 공통 Trade 로 변환해 집계기로.
					agg.AddTrade(ctx, common.Trade{
						Exchange: exchange,
						Code:     t.Code,
						Price:    t.TradePrice,
						Volume:   t.TradeVolume,
						TsMillis: t.Timestamp,
					})
				}
			case "ticker":
				var t upbitTicker
				if json.Unmarshal(msg, &t) == nil {
					// 업비트 원본 → 공통 TickerData 로 변환해 저장.
					w.SaveTicker(ctx, common.TickerData{
						Exchange:          exchange,
						Code:              t.Code,
						TradePrice:        t.TradePrice,
						Change:            t.Change,
						ChangeRate:        t.ChangeRate, // 이미 절댓값
						SignedChangeRate:  t.SignedChangeRate,
						SignedChangePrice: t.SignedChangePrice,
						AccTradePrice24h:  t.AccTradePrice24h,
						AccTradeVolume24h: t.AccTradeVolume24h,
						HighPrice:         t.HighPrice,
						LowPrice:          t.LowPrice,
						Timestamp:         t.Timestamp,
					})
				}
			}
		}
		conn.Close()
	}
}

// runRestTickerPoller : 52주 고저·전일종가를 1분마다 폴링해 Hash 저장.
func runRestTickerPoller(ctx context.Context, w *common.Writer, codes []string) {
	poll := func() {
		for i := 0; i < len(codes); i += restTickerBatch {
			select {
			case <-ctx.Done():
				return
			default:
			}
			end := i + restTickerBatch
			if end > len(codes) {
				end = len(codes)
			}
			url := restTickerURL + "?markets=" + strings.Join(codes[i:end], ",")
			body, err := fetch(url)
			if err != nil {
				log.Println("[upbit] REST ticker 실패:", err)
				continue
			}
			var tickers []upbitRestTicker
			if json.Unmarshal(body, &tickers) != nil {
				continue
			}
			for _, t := range tickers {
				w.SaveRestTicker(ctx, exchange, t.Market, map[string]interface{}{
					"exchange":              exchange,
					"trade_price":           t.TradePrice,
					"prev_closing_price":    t.PrevClosingPrice,
					"highest_52_week_price": t.Highest52WeekPrice,
					"highest_52_week_date":  t.Highest52WeekDate,
					"lowest_52_week_price":  t.Lowest52WeekPrice,
					"lowest_52_week_date":   t.Lowest52WeekDate,
				})
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	poll() // 시작 즉시 1회
	ticker := time.NewTicker(restTickerEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}
