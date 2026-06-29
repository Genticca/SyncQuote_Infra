// ============================================================================
// common/types.go — 두 거래소가 공유하는 "공통 데이터 구조"
// ----------------------------------------------------------------------------
// 통합의 핵심: 업비트든 바이낸스든, 거래소별 어댑터가 자기 거래소의 원본
// JSON을 파싱한 뒤 "반드시 이 공통 구조로 변환"해서 넘긴다. 그래야 Redis
// 저장 로직(writer.go)이 거래소를 구분하지 않고 한 벌로 처리할 수 있다.
//
// 즉, 거래소마다 다른 것 = "원본 → 공통구조 변환"(어댑터 담당)
//     거래소마다 같은 것 = "공통구조 → Redis 저장"(이 공통부 담당)
// ============================================================================

package common

// SecondCandle : 1초봉(OHLC). 모든 거래소가 이 구조로 통일된다.
// Exchange 필드가 데이터에 "꼬리표"처럼 따라다니며, Redis/MySQL/백엔드 전
// 구간에서 업비트/바이낸스를 구분하는 기준이 된다.
type SecondCandle struct {
	Exchange string  `json:"exchange"` // "upbit" / "binance"
	Code     string  `json:"code"`     // 종목 코드 (KRW-BTC / BTCUSDT)
	Time     int64   `json:"time"`     // 초 시작 시각 (Unix SECONDS) ※ TickerData.Timestamp는 ms라 단위 다름
	Open     float64 `json:"open"`     // 시가
	High     float64 `json:"high"`     // 고가
	Low      float64 `json:"low"`      // 저가
	Close    float64 `json:"close"`    // 종가
	Volume   float64 `json:"volume"`   // 거래량 합
}

// MarketInfo : 페어 정보. 거래소마다 가진 필드가 다르지만(업비트는 한글명·
// 경보가 있고 바이낸스는 없음), 구조를 통일하기 위해 같은 필드 집합을 쓰고
// 없는 값은 빈 값으로 둔다.
type MarketInfo struct {
	Exchange      string `json:"exchange"`
	Market        string `json:"market"`
	KoreanName    string `json:"korean_name"`    // 바이낸스는 "" 
	EnglishName   string `json:"english_name"`
	MarketWarning string `json:"market_warning"` // 바이낸스는 "NONE"
	EventWarning  bool   `json:"event_warning"`
	EventCaution  bool   `json:"event_caution"`
}

// TickerData : 현재가(실시간). 거래소 원본의 등락률 단위/부호 규칙이 달라도
// 어댑터가 아래 규칙으로 맞춰서 채운다.
//   - ChangeRate       : 절댓값 (부호 없음) — 두 거래소 통일
//   - SignedChangeRate : 부호 있는 변화율(소수). 화면 +2.34% 표시는 이걸 사용
//   - Change           : "RISE" / "EVEN" / "FALL"
type TickerData struct {
	Exchange          string
	Code              string
	TradePrice        float64 // 현재가
	Change            string  // RISE/EVEN/FALL
	ChangeRate        float64 // 변화율 절댓값
	SignedChangeRate  float64 // 부호 있는 변화율(소수)
	SignedChangePrice float64 // 부호 있는 변화액
	AccTradePrice24h  float64 // 24h 누적 거래대금
	AccTradeVolume24h float64 // 24h 누적 거래량
	HighPrice         float64 // 고가
	LowPrice          float64 // 저가
	Timestamp         int64   // 시각(Unix MILLISECONDS) ※ SecondCandle.Time은 초라 단위 다름. JOIN 시 주의
}
