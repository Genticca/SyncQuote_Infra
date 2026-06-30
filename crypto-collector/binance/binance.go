// ============================================================================
// binance/binance.go — 바이낸스 어댑터
// ----------------------------------------------------------------------------
// 역할: 바이낸스 "원본 JSON"을 파싱해 common 패키지의 "공통 구조"로 변환한다.
//   - 심볼 선정(REST)        → common.MarketInfo (markets:binance)
//   - 1초봉(WebSocket kline_1s) → common.SecondCandle  (봉 마감 시에만)
//   - 현재가(WebSocket ticker)  → common.TickerData
//
// 바이낸스 고유 특징:
//   - 1초봉을 "완성해서 줌" → 집계기 불필요. k.x(봉 마감)일 때만 저장.
//   - 가격이 "문자열"로 옴 → parseFloat 필요.
//   - 등락률 P가 퍼센트(부호 포함) → /100 후, change_rate는 절댓값으로 통일.
//   - 결합 스트림(/stream?streams=a/b/c)으로 여러 심볼을 한 연결에 묶음.
// ============================================================================

package binance

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"crypto-collector/common"
)

const exchange = "binance"

const (
	symbolsPerConn = 50 // 연결당 심볼 수 (심볼당 kline+ticker = 2스트림)
	// → USDT 페어 전체(수백 개)를 수집하므로 연결이 10개 안팎 생긴다.
	//   바이낸스 한 연결당 권장 상한은 1024 스트림(=512심볼)이라 50심볼이면 여유.

	// 바이낸스 공개 시장데이터 전용 도메인.
	// 일부 지역에서 api.binance.com / stream.binance.com 이 차단될 때,
	// 인증 불필요한 시장데이터는 아래 binance.vision 도메인으로 우회한다.
	//   REST:  data-api.binance.vision
	//   WS:    data-stream.binance.vision
	exchangeInfoURL = "https://data-api.binance.vision/api/v3/exchangeInfo"
	ticker24hURL    = "https://data-api.binance.vision/api/v3/ticker/24hr"
	wsBase          = "wss://data-stream.binance.vision/stream?streams="
)

// --- 바이낸스 원본 JSON 구조체 ---

type exchangeInfo struct {
	Symbols []struct {
		Symbol     string `json:"symbol"`
		Status     string `json:"status"`
		BaseAsset  string `json:"baseAsset"`
		QuoteAsset string `json:"quoteAsset"`
	} `json:"symbols"`
}

type ticker24h struct {
	Symbol      string      `json:"symbol"`
	QuoteVolume json.Number `json:"quoteVolume"` // 숫자/문자열 모두 수용
}

type combinedMessage struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

type klineMessage struct {
	Symbol string `json:"s"`
	Kline  struct {
		StartTime int64       `json:"t"`
		Open      json.Number `json:"o"` // json.Number: 숫자/문자열 둘 다 수용
		Close     json.Number `json:"c"`
		High      json.Number `json:"h"`
		Low       json.Number `json:"l"`
		Volume    json.Number `json:"v"`
		IsClosed  bool        `json:"x"` // ★ 봉 마감 여부

		// 대소문자 fold 충돌 흡수용 (값 미사용). 없으면 페이로드 뒤쪽의
		// "T"(봉 종료시각)가 StartTime("t")을, "V"(테이커 매수 base 거래량)가 Volume("v")을 덮어쓴다.
		CloseTime    int64       `json:"T"` // 봉 종료시각    — StartTime("t") 오염 방지
		TakerBuyBase json.Number `json:"V"` // 테이커 매수 거래량 — Volume("v") 오염 방지
		LastTradeID  int64       `json:"L"` // 마지막 체결 ID  — Low("l") 방어(순서상 무해하나 명시)
	} `json:"k"`
}

type binanceTicker struct {
	EventType      string      `json:"e"` // "24hrTicker" — E와의 대소문자 충돌을 막기 위해 명시
	Symbol         string      `json:"s"`
	LastPrice      json.Number `json:"c"` // json.Number: 도메인에 따라 숫자/문자열로 와도 수용
	PriceChange    json.Number `json:"p"`
	PriceChangePct json.Number `json:"P"`
	HighPrice      json.Number `json:"h"`
	LowPrice       json.Number `json:"l"`
	BaseVolume     json.Number `json:"v"`
	QuoteVolume    json.Number `json:"q"`
	EventTime      int64       `json:"E"`

	// 대소문자 fold 충돌 흡수용 (값 자체는 사용하지 않아도 됨).
	// Go encoding/json은 정확 일치 필드가 없으면 "대소문자 무시"로 첫 일치 필드에 값을 넣는다.
	// 아래 전용 필드가 없으면 페이로드 뒤쪽의 "C"(통계창 종료시각=현재 ms)가 LastPrice("c")를,
	// "L"(마지막 체결 ID)이 LowPrice("l")를 덮어써 현재가/저가가 타임스탬프·ID로 오염된다.
	StatCloseTime json.Number `json:"C"` // 통계창 종료시각(ms) — LastPrice("c") 오염 방지
	LastTradeID   json.Number `json:"L"` // 마지막 체결 ID     — LowPrice("l") 오염 방지
	LastQty       json.Number `json:"Q"` // 마지막 체결 수량   — QuoteVolume("q") 순서 의존성 제거
}

func fetch(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// parseFloat : json.Number(숫자/문자열 둘 다 수용)를 float64로. 실패 시 0.
// 바이낸스는 도메인에 따라 가격을 문자열("60442.99")로도, 숫자(60442.99)로도
// 보낸다. json.Number로 받으면 양쪽 다 안전하게 처리된다.
func parseFloat(n json.Number) float64 {
	f, _ := strconv.ParseFloat(n.String(), 64)
	return f
}

// Run : 바이낸스 수집 전체 실행. main이 goroutine으로 호출한다.
// wg에 내부 연결 goroutine들을 등록해, 종료 시 main이 완료를 기다릴 수 있게 한다.
func Run(ctx context.Context, w *common.Writer, wg *sync.WaitGroup) {
	symbols, err := selectSymbols(ctx, w)
	if err != nil {
		log.Println("[binance] 심볼 선정 실패:", err)
		return
	}
	log.Printf("[binance] 심볼 %d개 선정\n", len(symbols))

	// 심볼을 묶음으로 나눠 각 묶음마다 별도 연결(goroutine). 각각 wg에 등록.
	for i := 0; i < len(symbols); i += symbolsPerConn {
		end := i + symbolsPerConn
		if end > len(symbols) {
			end = len(symbols)
		}
		batch := symbols[i:end]
		wg.Add(1)
		go func() { defer wg.Done(); runOneConn(ctx, w, batch) }()
	}
}

// selectSymbols : USDT 페어 "전체"를 거래량 내림차순으로 수집 후 페어 저장.
// (이전에는 교집합 우선 + 거래량 상위 100개로 제한했으나, 전체 수집으로 변경)
// 거래량 순 정렬은 유지 — 연결 분배가 균형 잡히고 화면에서도 자연스럽다.
func selectSymbols(ctx context.Context, w *common.Writer) ([]string, error) {
	// 1) 바이낸스 USDT 현물(TRADING) 심볼.
	body, err := fetch(exchangeInfoURL)
	if err != nil {
		return nil, err
	}
	var info exchangeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	usdtSymbols := make(map[string]bool)
	for _, s := range info.Symbols {
		if s.QuoteAsset == "USDT" && s.Status == "TRADING" {
			usdtSymbols[s.Symbol] = true
		}
	}

	// 2) 거래량 내림차순 정렬 (전체를 정렬하되 자르지 않는다).
	body, err = fetch(ticker24hURL)
	if err != nil {
		return nil, err
	}
	var tickers []ticker24h
	if err := json.Unmarshal(body, &tickers); err != nil {
		return nil, err
	}
	type volPair struct {
		symbol string
		vol    float64
	}
	var ranked []volPair
	for _, t := range tickers {
		if usdtSymbols[t.Symbol] {
			ranked = append(ranked, volPair{t.Symbol, parseFloat(t.QuoteVolume)})
		}
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].vol > ranked[j].vol })

	// 3) 전체를 chosen에 담는다 (개수 제한 없음).
	chosen := make([]string, 0, len(ranked))
	for _, rp := range ranked {
		chosen = append(chosen, rp.symbol)
	}

	// 4) 페어 정보를 공통 MarketInfo로 저장.
	var infos []common.MarketInfo
	for _, sym := range chosen {
		base := strings.TrimSuffix(sym, "USDT")
		infos = append(infos, common.MarketInfo{
			Exchange:      exchange,
			Market:        sym,
			KoreanName:    "",
			EnglishName:   base,
			MarketWarning: "NONE",
		})
	}
	if err := w.SaveMarketsBatch(ctx, infos); err != nil {
		return nil, err
	}
	return chosen, nil
}

// runOneConn : 심볼 묶음 하나를 담당하는 WebSocket 연결. 끊기면 재연결.
func runOneConn(ctx context.Context, w *common.Writer, symbols []string) {
	var streams []string
	for _, s := range symbols {
		low := strings.ToLower(s)
		streams = append(streams, low+"@kline_1s", low+"@ticker")
	}
	url := wsBase + strings.Join(streams, "/")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			log.Println("[binance] WS 연결 실패, 3초 후 재시도:", err)
			time.Sleep(3 * time.Second)
			continue
		}
		log.Printf("[binance] WS 연결 성공 (%d심볼)\n", len(symbols))

		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			default:
			}

			// 읽기에 1초 deadline. 메시지가 없어도 1초마다 깨어나 ctx 확인.
			conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue // deadline 초과는 정상 — ctx 확인하러 위로
				}
				log.Println("[binance] 연결 끊김, 재연결:", err)
				break
			}

			var wrap combinedMessage
			if json.Unmarshal(msg, &wrap) != nil {
				continue
			}

			switch {
			case strings.HasSuffix(wrap.Stream, "@kline_1s"):
				var k klineMessage
				if json.Unmarshal(wrap.Data, &k) == nil {
					saveKline(ctx, w, k)
				}
			case strings.HasSuffix(wrap.Stream, "@ticker"):
				var t binanceTicker
				if json.Unmarshal(wrap.Data, &t) == nil {
					saveTicker(ctx, w, t)
				}
			}
		}
		conn.Close()
	}
}

// saveKline : 마감된 1초봉만 공통 SecondCandle로 변환해 저장.
func saveKline(ctx context.Context, w *common.Writer, k klineMessage) {
	if !k.Kline.IsClosed {
		return // 미완성 봉 무시
	}
	w.SaveCandle(ctx, common.SecondCandle{
		Exchange: exchange,
		Code:     k.Symbol,
		Time:     k.Kline.StartTime / 1000, // ms → s
		Open:     parseFloat(k.Kline.Open),
		High:     parseFloat(k.Kline.High),
		Low:      parseFloat(k.Kline.Low),
		Close:    parseFloat(k.Kline.Close),
		Volume:   parseFloat(k.Kline.Volume),
	})
}

// saveTicker : 현재가를 공통 TickerData로 변환해 저장. (단위·부호 통일)
func saveTicker(ctx context.Context, w *common.Writer, t binanceTicker) {
	signed := parseFloat(t.PriceChangePct) / 100.0 // 2.34(%) → 0.0234

	abs := signed // change_rate는 절댓값으로 통일 (업비트와 동일 규칙)
	if abs < 0 {
		abs = -abs
	}

	change := "EVEN"
	switch {
	case signed > 0:
		change = "RISE"
	case signed < 0:
		change = "FALL"
	}

	w.SaveTicker(ctx, common.TickerData{
		Exchange:          exchange,
		Code:              t.Symbol,
		TradePrice:        parseFloat(t.LastPrice),
		Change:            change,
		ChangeRate:        abs,    // 절댓값
		SignedChangeRate:  signed, // 부호 있음
		SignedChangePrice: parseFloat(t.PriceChange),
		AccTradePrice24h:  parseFloat(t.QuoteVolume),
		AccTradeVolume24h: parseFloat(t.BaseVolume),
		HighPrice:         parseFloat(t.HighPrice),
		LowPrice:          parseFloat(t.LowPrice),
		Timestamp:         t.EventTime,
	})
}
