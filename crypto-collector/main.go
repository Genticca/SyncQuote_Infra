// ============================================================================
// main.go — 통합 수집기 진입점
// ----------------------------------------------------------------------------
// 하나의 프로그램으로 업비트와 바이낸스를 "동시에" 수집한다(A: 한 번에 실행).
// 두 거래소는 각자 어댑터(upbit.Run / binance.Run)를 goroutine으로 돌리고,
// 같은 공통 저장기(common.Writer)를 공유해 Redis에 적재한다(B: 코드 통합).
//
// 데이터는 코드가 합쳐져도 섞이지 않는다:
//   - 각 캔들·현재가에 Exchange 필드가 박혀 있고
//   - 키가 candles:1s:upbit:... / candles:1s:binance:... 로 갈라지므로
//   Redis→MySQL→백엔드 전 구간에서 거래소별로 분리돼 흐른다.
//
// [기동하는 goroutine 들 — 모두 WaitGroup으로 추적]
//   1) upbit.Run    : 업비트 수집 (집계기 + WS + REST폴링)
//   2) binance.Run  : 바이낸스 수집 (WS 연결 여러 개)
//   3) archiver.Run : 60초마다 Redis→MySQL 아카이빙 (1분봉 집계)
//   → 종료 시 main이 wg.Wait()로 이 모두가 깔끔히 끝날 때까지 기다린다.
//
// [MySQL은 선택적]
//   Redis는 필수(없으면 종료). MySQL은 없어도 수집은 계속되고 아카이빙만
//   비활성된다. → 개발 중 MySQL 없이도 수집 동작을 확인할 수 있다.
// ============================================================================

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"crypto-collector/binance"
	"crypto-collector/common"
	"crypto-collector/upbit"
)

// getEnv : 환경변수를 읽되, 없거나 비었으면 fallback(기본값)을 반환한다.
// 설정(접속 주소·비밀번호 등)을 코드 밖으로 빼 환경마다 유연하게 바꾸기 위함.
func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func main() {
	// Redis 연결 (업비트·바이낸스가 같은 Redis를 공유).
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, DB: 0})
	defer rdb.Close()

	baseCtx := context.Background()
	if err := rdb.Ping(baseCtx).Err(); err != nil {
		log.Fatal("Redis 연결 실패:", err)
	}
	log.Println("Redis 연결 성공!")

	// Ctrl+C/종료 신호를 모든 goroutine에 전파하는 context.
	ctx, stop := signal.NotifyContext(baseCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// MySQL 연결 (선택적 — 실패해도 수집은 계속, 아카이빙만 비활성).
	// 테이블은 팀원 SQL(crypto_data 스키마)로 "수동 생성"하므로, 여기서
	// 테이블을 만들지 않는다. 연결만 확인한다.
	mysqlDSN := buildMySQLDSN()
	dbConn, err := common.ConnectMySQL(ctx, mysqlDSN)
	if err != nil {
		log.Printf("MySQL 연결 실패 (수집은 계속, 아카이빙 비활성): %v", err)
		dbConn = nil
	} else {
		defer dbConn.Close()
		log.Println("MySQL 연결 성공!")
	}

	// 공통 저장기 한 개를 두 어댑터가 공유.
	writer := common.NewWriter(rdb)

	// WaitGroup: 두 어댑터가 띄우는 내부 goroutine들의 완료를 추적한다.
	var wg sync.WaitGroup

	// 두 거래소를 동시에 수집. 각 Run이 자기 내부 goroutine을 wg에 등록한다.
	upbit.Run(ctx, writer, &wg)
	binance.Run(ctx, writer, &wg)

	// Redis → MySQL 60초 배치 아카이빙. (TTL 90초 > 주기 60초 안전마진)
	archiver := common.NewArchiver(rdb, dbConn)
	wg.Add(1)
	go func() { defer wg.Done(); archiver.Run(ctx, 60*time.Second) }()

	log.Println("통합 수집 중! 업비트 + 바이낸스 + 아카이빙 (종료: Ctrl+C)")

	// 종료 신호 대기.
	<-ctx.Done()
	log.Println("종료 중... (goroutine 정리 대기)")

	// 모든 goroutine이 ctx.Done()을 감지하고 실제로 빠져나올 때까지 대기.
	wg.Wait()
	log.Println("종료 완료")
}

// buildMySQLDSN : 환경변수로 DSN 조립. MYSQL_DSN이 있으면 그대로 사용.
func buildMySQLDSN() string {
	if dsn := os.Getenv("MYSQL_DSN"); dsn != "" {
		return dsn
	}
	host := getEnv("MYSQL_HOST", "127.0.0.1")
	port := getEnv("MYSQL_PORT", "3306")
	user := getEnv("MYSQL_USER", "syncquote")
	pass := getEnv("MYSQL_PASSWORD", "syncquote")
	dbName := getEnv("MYSQL_DATABASE", "syncquote")
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true", user, pass, host, port, dbName)
}
