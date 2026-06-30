// ============================================================================
// common/keys.go — Redis 키 조립 (모든 거래소 공통)
// ----------------------------------------------------------------------------
// 통합 전에는 업비트/바이낸스 파일이 각자 exchange 상수를 박아 키를 만들었다.
// 통합 후에는 exchange를 "파라미터로 받아" 키를 만든다. 이렇게 하면 키 패턴이
// 한 곳에서만 관리되고, 거래소가 늘어도 이 파일은 손댈 필요가 없다.
//
// 패턴 = {종류}:{시간단위}:{거래소}:{심볼}
//   - 거래소가 심볼 앞 → SCAN candles:1s:upbit:* 처럼 거래소별 조회가 깔끔
// ============================================================================

package common

// SecondCandleKey : 1초봉 List 키. 예) candles:1s:upbit:KRW-BTC
// (API 실시간 차트용 캐시 — TTL/개수상한. 유실돼도 무해)
func SecondCandleKey(exchange, code string) string {
	return "candles:1s:" + exchange + ":" + code
}

// CandleStreamKey : 1초봉 영속화용 Stream 키 (아카이빙 전용, 단일 스트림).
// List와 분리해 용도별 정책을 따로 둔다: List=실시간(유실 무해),
// Stream=MySQL 적재 보장(아카이버가 ack할 때까지 보존).
func CandleStreamKey() string {
	return "candles:1s:stream"
}

// TickerKey : 실시간 현재가 Stream 키. 예) ticker:binance:BTCUSDT
func TickerKey(exchange, code string) string {
	return "ticker:" + exchange + ":" + code
}

// MarketsKey : 페어 목록 Hash 키. 예) markets:upbit
func MarketsKey(exchange string) string {
	return "markets:" + exchange
}

// RestTickerKey : 52주 메타 등 REST 전용 Hash 키. (업비트에서 사용)
// 예) ticker:rest:upbit:KRW-BTC
func RestTickerKey(exchange, code string) string {
	return "ticker:rest:" + exchange + ":" + code
}
