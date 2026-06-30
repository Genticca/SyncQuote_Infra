// ============================================================================
// common/health.go — K8s liveness/readiness용 경량 헬스 엔드포인트
// ----------------------------------------------------------------------------
// 수집기/아카이버는 인바운드 트래픽 포트가 없어 HTTP/exec Probe를 걸기 어렵다.
// 그래서 의존성(Redis, 아카이버는 MySQL까지) 연결 상태를 확인하는 최소 HTTP
// 서버(/healthz)를 띄워, K8s Probe가 이 엔드포인트를 찌르게 한다.
//   - 200 ok        : 의존성 정상
//   - 503           : Redis(또는 MySQL) 연결 불가
// db == nil(수집 전용 모드)이면 Redis만 확인한다.
// ============================================================================

package common

import (
	"context"
	"database/sql"
	"log"
	"net/http"

	"github.com/redis/go-redis/v9"
)

// StartHealthServer : addr(예 ":8080")에서 /healthz 를 제공한다. ctx가 끝나면 닫는다.
func StartHealthServer(ctx context.Context, rdb *redis.Client, db *sql.DB, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(r.Context()).Err(); err != nil {
			http.Error(w, "redis down", http.StatusServiceUnavailable)
			return
		}
		if db != nil {
			if err := db.PingContext(r.Context()); err != nil {
				http.Error(w, "mysql down", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	go func() {
		log.Printf("[health] /healthz 리슨 %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[health] 서버 종료: %v", err)
		}
	}()
}
