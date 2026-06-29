// ============================================================================
// common/aggregator.go — 체결(trade) → 1초봉 집계기
// ----------------------------------------------------------------------------
// "초봉을 직접 만들어야 하는 거래소"용 공용 도구.
//   - 업비트: 1초봉을 안 주므로 체결을 모아 직접 집계 → 이 집계기 사용.
//   - 바이낸스: 거래소가 1초봉(kline_1s)을 완성해 주므로 이 집계기 불필요.
//
// 나중에 trade 기반으로 초봉을 만들어야 하는 거래소가 또 생기면 이 집계기를
// 그대로 재사용한다. (그래서 업비트 전용이 아니라 common에 둔다)
//
// 동작 비유: "1초짜리 상자"에 그 초의 체결을 담다가, 다음 초가 되면 상자를
//           봉인(flush)해 Redis에 올리고 새 빈 상자를 꺼낸다.
// ============================================================================

package common

import (
	"context"
	"sync"
	"time"
)

// Trade : 어댑터가 체결 1건을 이 형태로 넘긴다. (거래소 원본에서 변환)
type Trade struct {
	Exchange string
	Code     string
	Price    float64
	Volume   float64
	TsMillis int64 // 체결 시각 (밀리초)
}

// Aggregator : 체결을 1초봉으로 모으는 집계기.
type Aggregator struct {
	mu      sync.Mutex
	current map[string]*SecondCandle // 종목코드 → 만들고 있는 초봉
	writer  *Writer
}

// NewAggregator : 집계기 생성.
func NewAggregator(w *Writer) *Aggregator {
	return &Aggregator{
		current: make(map[string]*SecondCandle),
		writer:  w,
	}
}

// AddTrade : 체결 1건을 현재 초봉에 반영.
//
// [락 설계] Redis 저장(SaveCandle = 네트워크 I/O)을 mu.Lock() 안에서 호출하면,
// 그 I/O가 끝날 때까지 다른 종목의 체결 처리까지 모두 막힌다(락 경합). 그래서
// 확정할 봉은 락 안에서 "꺼내 두기만" 하고, 실제 저장은 락을 푼 뒤에 한다.
func (a *Aggregator) AddTrade(ctx context.Context, t Trade) {
	sec := t.TsMillis / 1000 // ms → s

	var toSave *SecondCandle // 락 밖에서 저장할 확정봉 (없으면 nil)

	a.mu.Lock()
	cur, exists := a.current[t.Code]

	// 초가 바뀌었으면 이전 초봉을 "확정 대상"으로 빼 둔다. (저장은 락 밖에서)
	if exists && cur.Time != sec {
		snapshot := *cur // 값 복사 — 락을 푼 뒤 map이 바뀌어도 안전
		toSave = &snapshot
		exists = false
	}

	// [과거 trade 방어] 재연결 등으로 이미 확정된 초보다 더 과거의 체결이
	// 뒤늦게 도착할 수 있다. 그런 체결로 새 초봉을 만들면 과거 시각의 봉이
	// 다시 생겨 1분봉 집계를 오염시키므로, 진행 중인 봉보다 과거면 무시한다.
	if exists && sec < cur.Time {
		a.mu.Unlock()
		return
	}

	switch {
	case !exists:
		// 만들던 초봉이 없으면 새로 시작 (첫 체결가 = 시가).
		a.current[t.Code] = &SecondCandle{
			Exchange: t.Exchange,
			Code:     t.Code,
			Time:     sec,
			Open:     t.Price,
			High:     t.Price,
			Low:      t.Price,
			Close:    t.Price,
			Volume:   t.Volume,
		}
	default:
		// 기존 초봉 갱신.
		if t.Price > cur.High {
			cur.High = t.Price
		}
		if t.Price < cur.Low {
			cur.Low = t.Price
		}
		cur.Close = t.Price
		cur.Volume += t.Volume
	}
	a.mu.Unlock()

	// 락을 푼 뒤 저장 — I/O가 다른 종목 처리를 막지 않는다.
	if toSave != nil {
		a.writer.SaveCandle(ctx, *toSave)
	}
}

// RunFlushLoop : 거래가 뜸한 종목의 미완성 초봉을 1초마다 강제 확정.
// (다음 체결이 안 오면 초봉이 닫히지 않고 남는 문제를 해결)
func (a *Aggregator) RunFlushLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 확정할 봉들을 락 안에서 모아서 빼낸 뒤, 락을 푼 다음 저장한다.
			// (AddTrade와 동일한 이유 — I/O를 락 밖으로)
			a.mu.Lock()
			now := time.Now().Unix()
			var toSave []SecondCandle
			for code, cur := range a.current {
				if cur.Time < now {
					toSave = append(toSave, *cur) // 값 복사
					delete(a.current, code)
				}
			}
			a.mu.Unlock()

			for i := range toSave {
				a.writer.SaveCandle(ctx, toSave[i])
			}
		}
	}
}
