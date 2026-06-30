-- mysql-exporter-user.sql
-- sq-mysql-01(172.16.116.100)에서 실행. mysqld_exporter 전용 최소권한 계정 생성.
-- exporter는 같은 VM에서 localhost로 접속하므로 호스트는 'localhost'로 한정(원격 로그인 차단).
--   실행: mysql -u root -p < mysql-exporter-user.sql
-- 'CHANGE_ME'는 exporter.my.cnf 의 password와 동일하게.

CREATE USER IF NOT EXISTS 'exporter'@'localhost'
  IDENTIFIED BY 'CHANGE_ME'
  WITH MAX_USER_CONNECTIONS 3;          -- exporter가 연결을 독식하지 않도록 제한

GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'localhost';
FLUSH PRIVILEGES;
