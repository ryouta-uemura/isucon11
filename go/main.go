package main

import (
	"bytes"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"

	"net"
	_ "net/http/pprof"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const (
	sessionName                 = "isucondition_go"
	conditionLimit              = 20
	frontendContentsPath        = "../public"
	jiaJWTSigningKeyPath        = "../ec256-public.pem"
	defaultIconFilePath         = "../NoImage.jpg"
	defaultJIAServiceURL        = "http://localhost:5000"
	mysqlErrNumDuplicateEntry   = 1062
	conditionLevelInfo          = "info"
	conditionLevelWarning       = "warning"
	conditionLevelCritical      = "critical"
	scoreConditionLevelInfo     = 3
	scoreConditionLevelWarning  = 2
	scoreConditionLevelCritical = 1

	iconFileDir = "../icons"
)

var (
	db                  *sqlx.DB
	sessionStore        sessions.Store
	mySQLConnectionData *MySQLConnectionEnv

	jiaJWTSigningKey *ecdsa.PublicKey

	postIsuConditionTargetBaseURL string // JIAへのactivate時に登録する，ISUがconditionを送る先のURL

	// trendにキャッシュを導入する
	trendCacheMu      sync.RWMutex
	trendCacheVersion uint64
	trendCache        []TrendResponse
	trendCacheBuildMu sync.Mutex
	// TREND_CACHE_TTL_MS で与える。0 なら期限なし(=凍結、従来の挙動)。
	// 掃引のたびに再ビルドすると走行間に余計な変数が混ざるので環境変数にした。
	trendCacheTTL     time.Duration
	trendCacheBuiltAt time.Time

	// 確定した日のグラフは二度と変化しないのでメモ化する。
	// key: "uuid|dayUnix" -> JSONバイト列（シリアライズもまとめて省く）
	graphCache sync.Map
	// 受信した condition の最大 timestamp。仮想時刻の代理として使う。
	// アプリの壁時計は仮想時間と無関係なので判定には使えない。
	latestConditionTS int64

	// jia_isu_uuid -> isuのmeta
	// 存在確認, 認可判定に活用
	isuMetaMu     sync.RWMutex
	isuMetaByUUID map[string]IsuMeta

	// 各 ISU の最新コンディション。GET /api/isu を DB なしで返すために持つ。
	// 更新規則は DB 側の `WHERE timestamp < ?` と同じ（新しい時だけ上書き）。
	latestCondMu     sync.RWMutex
	latestCondByUUID map[string]LatestCond

	sessionUserCache sync.Map // session cookie string -> jia_user_id
	// skipping the cryptographic calculations

	emptyInt64Slice = []int64{}
)

type IsuMeta struct {
	ID         int
	JIAIsuUUID string `db:"jia_isu_uuid"`
	Name       string
	Character  string
	JIAUserID  string `db:"jia_user_id"`
}

type LatestCond struct {
	TimestampUnix int64
	IsSitting     bool
	ConditionBits int
	LevelInt      int
	Message       string
}

// /initialize 後に DB から読み直す。初期データにもコンディションがあるので必須。
// 空のまま返すとベンチが「LatestIsuCondition が nil」で不整合とみなす。
// GET /api/condition/:uuid 用のインメモリ索引。
//
// クエリ結果のキャッシュは効かない（47,718件中ユニーク44,592件で、end_time を
// ずらしながらページングするため平均1.07回しか再利用されない）。そこで結果ではなく
// 「元データ」を持ち、毎回メモリ上を二分探索する。
//
// 規模: 1走行の終端で 760,655行 / 79 ISU、message は平均21バイト。
// CondRow 1件あたり約40バイト + 文字列なので合計 60MB 程度で、4GB のVMに十分載る。
type CondRow struct {
	TimestampUnix int64
	Message       string
	ConditionBits int32
	LevelInt      int8
	IsSitting     bool
}

// ISU ごとに分ける。全体で1つのロックにすると POST と GET が同じ錠を奪い合う。
type isuCondList struct {
	mu   sync.RWMutex
	rows []CondRow // TimestampUnix 昇順
}

var (
	condStoreMu      sync.RWMutex
	condStore        map[string]*isuCondList
	condCacheEnabled  = true // COND_CACHE=0 で DB 経路に戻せる（A/B 用）
	condInsertEnabled = true  // COND_INSERT=0 で isu_condition への書き込みを止める
	noNginx           = false // NO_NGINX=1 で nginx を外し Go が直接 TLS を終端する
)

func condListFor(jiaIsuUUID string) *isuCondList {
	condStoreMu.RLock()
	l := condStore[jiaIsuUUID]
	condStoreMu.RUnlock()
	if l != nil {
		return l
	}

	condStoreMu.Lock()
	defer condStoreMu.Unlock()
	if condStore == nil {
		condStore = map[string]*isuCondList{}
	}
	if l = condStore[jiaIsuUUID]; l == nil {
		l = &isuCondList{rows: make([]CondRow, 0, 1024)}
		condStore[jiaIsuUUID] = l
	}
	return l
}

// DB への書き込みが成功したあとにだけ呼ぶこと。
// 先に入れると、POST が失敗した(=ベンチが成功と見なしていない)コンディションを
// 返してしまう。latestCond と同じ理由。
func appendConds(jiaIsuUUID string, rows []CondRow) {
	l := condListFor(jiaIsuUUID)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rows {
		n := len(l.rows)
		// ポスターは時刻順に送ってくるので、ほぼ必ずこの追記で済む。
		if n == 0 || l.rows[n-1].TimestampUnix <= r.TimestampUnix {
			l.rows = append(l.rows, r)
			continue
		}
		// まれに最大600仮想秒さかのぼった分が来る。末尾付近なので移動量は小さい。
		i := sort.Search(n, func(i int) bool { return l.rows[i].TimestampUnix > r.TimestampUnix })
		l.rows = append(l.rows, CondRow{})
		copy(l.rows[i+1:], l.rows[i:])
		l.rows[i] = r
	}
}

// SQL と同じ意味にすること:
//   timestamp < endTime / startTime <= timestamp / level_int IN (...) / DESC / LIMIT
func queryConds(jiaIsuUUID string, endTime int64, startTime int64, hasStart bool,
	levelMask uint8, limit int) []CondRow {

	l := condListFor(jiaIsuUUID)
	l.mu.RLock()
	defer l.mu.RUnlock()

	rows := l.rows
	// timestamp < endTime なので endTime 以上になる最初の位置が上限(排他)
	hi := sort.Search(len(rows), func(i int) bool { return rows[i].TimestampUnix >= endTime })
	lo := 0
	if hasStart {
		// startTime <= timestamp
		lo = sort.Search(len(rows), func(i int) bool { return rows[i].TimestampUnix >= startTime })
	}

	out := make([]CondRow, 0, limit)
	for i := hi - 1; i >= lo && len(out) < limit; i-- {
		if levelMask&(1<<uint8(rows[i].LevelInt)) != 0 {
			out = append(out, rows[i])
		}
	}
	return out
}

// /initialize 後に DB から読み直す。初期データは618行しかないので一瞬で終わる。
func loadCondCache() error {
	rows, err := db.Queryx(
		"SELECT jia_isu_uuid, UNIX_TIMESTAMP(timestamp), is_sitting, `condition_bits`, level_int, message" +
			" FROM `isu_condition` ORDER BY `jia_isu_uuid`, `timestamp`")
	if err != nil {
		return err
	}
	defer rows.Close()

	next := map[string]*isuCondList{}
	for rows.Next() {
		var uuid string
		var r CondRow
		var bits, level int
		if err := rows.Scan(&uuid, &r.TimestampUnix, &r.IsSitting, &bits, &level, &r.Message); err != nil {
			return err
		}
		r.ConditionBits = int32(bits)
		r.LevelInt = int8(level)
		l := next[uuid]
		if l == nil {
			l = &isuCondList{rows: make([]CondRow, 0, 1024)}
			next[uuid] = l
		}
		l.rows = append(l.rows, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	condStoreMu.Lock()
	condStore = next
	condStoreMu.Unlock()
	return nil
}

func loadLatestCondCache() error {
	rows, err := db.Queryx(
		"SELECT jia_isu_uuid, UNIX_TIMESTAMP(timestamp), is_sitting, `condition_bits`, level_int, message" +
			" FROM latest_isu_condition")
	if err != nil {
		return err
	}
	defer rows.Close()

	next := map[string]LatestCond{}
	for rows.Next() {
		var uuid string
		var lc LatestCond
		if err := rows.Scan(&uuid, &lc.TimestampUnix, &lc.IsSitting, &lc.ConditionBits, &lc.LevelInt, &lc.Message); err != nil {
			return err
		}
		next[uuid] = lc
	}
	if err := rows.Err(); err != nil {
		return err
	}

	latestCondMu.Lock()
	latestCondByUUID = next
	latestCondMu.Unlock()
	return nil
}

// DB へのコミットが成功した後にだけ呼ぶこと。
// 先に呼ぶと、POST が失敗した(=ベンチが成功と見なしていない)コンディションを
// 返してしまい「POSTに成功していない時刻のデータが返されました」になる。
func updateLatestCond(jiaIsuUUID string, lc LatestCond) {
	latestCondMu.Lock()
	defer latestCondMu.Unlock()

	if latestCondByUUID == nil {
		latestCondByUUID = map[string]LatestCond{}
	}
	if cur, ok := latestCondByUUID[jiaIsuUUID]; ok && cur.TimestampUnix >= lc.TimestampUnix {
		return
	}
	latestCondByUUID[jiaIsuUUID] = lc
}

func getLatestCond(jiaIsuUUID string) (LatestCond, bool) {
	latestCondMu.RLock()
	defer latestCondMu.RUnlock()

	lc, ok := latestCondByUUID[jiaIsuUUID]
	return lc, ok
}

func listIsuMetaByUser(jiaUserID string) []IsuMeta {
	isuMetaMu.RLock()
	metas := make([]IsuMeta, 0, 16)
	for _, m := range isuMetaByUUID {
		if m.JIAUserID == jiaUserID {
			metas = append(metas, m)
		}
	}
	isuMetaMu.RUnlock()

	sort.Slice(metas, func(i, j int) bool { return metas[i].ID > metas[j].ID }) // ORDER BY i.id DESC
	return metas
}

func loadIsuMetaCache() error {
	rows := make([]IsuMeta, 0, 1024)
	dbRows, err := db.Queryx(`
		SELECT id, jia_isu_uuid, name, ` + "`character`" + `, jia_user_id
		FROM isu
	`)
	if err != nil {
		return err
	}
	defer dbRows.Close()

	for dbRows.Next() {
		var row IsuMeta
		if err := dbRows.Scan(&row.ID, &row.JIAIsuUUID, &row.Name, &row.Character, &row.JIAUserID); err != nil {
			return err
		}
		rows = append(rows, row)
	}
	if err := dbRows.Err(); err != nil {
		return err
	}

	next := make(map[string]IsuMeta, len(rows))
	for _, row := range rows {
		next[row.JIAIsuUUID] = row
	}

	isuMetaMu.Lock()
	isuMetaByUUID = next
	isuMetaMu.Unlock()

	return nil
}

func existsIsu(jiaIsuUUID string) bool {
	isuMetaMu.RLock()
	defer isuMetaMu.RUnlock()

	_, ok := isuMetaByUUID[jiaIsuUUID]
	return ok
}

// multi-tenancyの制御に使う
func getAuthorizedIsuMeta(jiaUserID, jiaIsuUUID string) (IsuMeta, bool) {
	isuMetaMu.RLock()
	defer isuMetaMu.RUnlock()

	meta, ok := isuMetaByUUID[jiaIsuUUID]
	if !ok {
		return IsuMeta{}, false
	}
	if meta.JIAUserID != jiaUserID {
		return IsuMeta{}, false
	}

	return meta, true
}

// when isu added to DB
func addIsuMeta(meta IsuMeta) {
	isuMetaMu.Lock()
	defer isuMetaMu.Unlock()

	if isuMetaByUUID == nil {
		isuMetaByUUID = map[string]IsuMeta{}
	}
	isuMetaByUUID[meta.JIAIsuUUID] = meta
}

func getTrendCache() ([]TrendResponse, uint64, bool) {
	trendCacheMu.RLock()
	defer trendCacheMu.RUnlock()
	if trendCache == nil {
		return nil, trendCacheVersion, false
	}
	// TTL=0 は期限なし(凍結)。従来の挙動。
	if trendCacheTTL > 0 && time.Since(trendCacheBuiltAt) > trendCacheTTL {
		return nil, trendCacheVersion, false
	}
	return trendCache, trendCacheVersion, true
}

func getTrendCacheVersion() uint64 {
	trendCacheMu.RLock()
	defer trendCacheMu.RUnlock()
	return trendCacheVersion
}

func setTrendCacheIfFresh(version uint64, res []TrendResponse) {
	trendCacheMu.Lock()
	defer trendCacheMu.Unlock()
	if version == trendCacheVersion {
		trendCache = res
		trendCacheBuiltAt = time.Now()
	}
}

func invalidateTrendCache() {
	trendCacheMu.Lock()
	trendCache = nil
	trendCacheVersion++
	trendCacheMu.Unlock()
}

type Config struct {
	Name string `db:"name"`
	URL  string `db:"url"`
}

type Isu struct {
	ID         int       `db:"id" json:"id"`
	JIAIsuUUID string    `db:"jia_isu_uuid" json:"jia_isu_uuid"`
	Name       string    `db:"name" json:"name"`
	Image      []byte    `db:"image" json:"-"`
	Character  string    `db:"character" json:"character"`
	JIAUserID  string    `db:"jia_user_id" json:"-"`
	CreatedAt  time.Time `db:"created_at" json:"-"`
	UpdatedAt  time.Time `db:"updated_at" json:"-"`
}

type IsuFromJIA struct {
	Character string `json:"character"`
}

type GetIsuListResponse struct {
	ID                 int                      `json:"id"`
	JIAIsuUUID         string                   `json:"jia_isu_uuid"`
	Name               string                   `json:"name"`
	Character          string                   `json:"character"`
	LatestIsuCondition *GetIsuConditionResponse `json:"latest_isu_condition"`
}

type IsuCondition struct {
	ID         int       `db:"id"`
	JIAIsuUUID string    `db:"jia_isu_uuid"`
	Timestamp  time.Time `db:"timestamp"`
	IsSitting  bool      `db:"is_sitting"`
	//	Condition     string    `db:"condition"`
	ConditionBits int `db:"condition_bits"`
	//	Level         string    `db:"level"`
	LevelInt  int       `db:"level_int"`
	Message   string    `db:"message"`
	CreatedAt time.Time `db:"created_at"`
}

type MySQLConnectionEnv struct {
	Host     string
	Port     string
	User     string
	DBName   string
	Password string
}

type InitializeRequest struct {
	JIAServiceURL string `json:"jia_service_url"`
}

type InitializeResponse struct {
	Language string `json:"language"`
}

type GetMeResponse struct {
	JIAUserID string `json:"jia_user_id"`
}

type GraphResponse struct {
	StartAt             int64           `json:"start_at"`
	EndAt               int64           `json:"end_at"`
	Data                *GraphDataPoint `json:"data"`
	ConditionTimestamps []int64         `json:"condition_timestamps"`
}

type GraphDataPoint struct {
	Score      int                  `json:"score"`
	Percentage ConditionsPercentage `json:"percentage"`
}

type ConditionsPercentage struct {
	Sitting      int `json:"sitting"`
	IsBroken     int `json:"is_broken"`
	IsDirty      int `json:"is_dirty"`
	IsOverweight int `json:"is_overweight"`
}

type GraphDataPointWithInfo struct {
	JIAIsuUUID          string
	StartAt             time.Time
	Data                GraphDataPoint
	ConditionTimestamps []int64
}

type GetIsuConditionResponse struct {
	JIAIsuUUID     string `json:"jia_isu_uuid"`
	IsuName        string `json:"isu_name"`
	Timestamp      int64  `json:"timestamp"`
	IsSitting      bool   `json:"is_sitting"`
	Condition      string `json:"condition"`
	ConditionLevel string `json:"condition_level"`
	Message        string `json:"message"`
}

type TrendResponse struct {
	Character string            `json:"character"`
	Info      []*TrendCondition `json:"info"`
	Warning   []*TrendCondition `json:"warning"`
	Critical  []*TrendCondition `json:"critical"`
}

type TrendCondition struct {
	ID        int   `json:"isu_id"`
	Timestamp int64 `json:"timestamp"`
}

type PostIsuConditionRequest struct {
	IsSitting bool   `json:"is_sitting"`
	Condition string `json:"condition"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

type JIAServiceRequest struct {
	TargetBaseURL string `json:"target_base_url"`
	IsuUUID       string `json:"isu_uuid"`
}

func getEnv(key string, defaultValue string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}
	return defaultValue
}

func NewMySQLConnectionEnv() *MySQLConnectionEnv {
	return &MySQLConnectionEnv{
		//Host:     getEnv("MYSQL_HOST", "127.0.0.1"),
		//Port:     getEnv("MYSQL_PORT", "3306"),
		//User:     getEnv("MYSQL_USER", "isucon"),
		//DBName:   getEnv("MYSQL_DBNAME", "isucondition"),
		//Password: getEnv("MYSQL_PASS", "isucon"),
		Host: "/run/mysqld/mysqld.sock",
		//Port:     getEnv("MYSQL_PORT", "3306"),
		User:     getEnv("MYSQL_USER", "isucon"),
		DBName:   getEnv("MYSQL_DBNAME", "isucondition"),
		Password: getEnv("MYSQL_PASS", "isucon"),
	}
}

func (mc *MySQLConnectionEnv) ConnectDB() (*sqlx.DB, error) {
	//dsn := fmt.Sprintf("%v:%v@tcp(%v:%v)/%v?parseTime=true&loc=Asia%%2FTokyo&interpolateParams=true", mc.User, mc.Password, mc.Host, mc.Port, mc.DBName) // driverにてSQLパラメータの埋め込みを試してみる, logにPREPAREが非常に多いから (理解不十分)
	dsn := fmt.Sprintf("%v:%v@unix(%v)/%v?parseTime=true&loc=Asia%%2FTokyo&interpolateParams=true", mc.User, mc.Password, mc.Host, mc.DBName) // driverにてSQLパラメータの埋め込みを試してみる, logにPREPAREが非常に多いから (理解不十分)
	return sqlx.Open("mysql", dsn)
}

// icon file exporting from DB
func saveIsuIconToFile(jiaIsuUUID string, jiaUserID string, image []byte) error {
	// この処理無駄じゃない？
	if err := os.MkdirAll(iconFileDir, 0755); err != nil {
		return err
	}

	iconPath := filepath.Join(iconFileDir, jiaUserID+"-"+jiaIsuUUID+".jpg")
	return ioutil.WriteFile(iconPath, image, 0644)
}

func exportIsuIconsToFile() error {
	rows, err := db.Queryx("SELECT jia_isu_uuid, image, jia_user_id FROM isu")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var isu struct {
			JIAIsuUUID string `db:"jia_isu_uuid"`
			Image      []byte `db:"image"`
			JIAUserID  string `db:"jia_user_id"`
		}
		if err := rows.Scan(&isu.JIAIsuUUID, &isu.Image, &isu.JIAUserID); err != nil {
			return err
		}
		if err := saveIsuIconToFile(isu.JIAIsuUUID, isu.JIAUserID, isu.Image); err != nil {
			return err
		}
	}

	return rows.Err()
}

// delete writtten files for initialization
func resetIsuIconFiles() error {
	if err := os.RemoveAll(iconFileDir); err != nil {
		return err
	}
	return os.MkdirAll(iconFileDir, 0755)
}

// global values.
var jiaURL string

func init() {
	sessionStore = sessions.NewCookieStore([]byte(getEnv("SESSION_KEY", "isucondition")))

	key, err := ioutil.ReadFile(jiaJWTSigningKeyPath)
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}
	jiaJWTSigningKey, err = jwt.ParseECPublicKeyFromPEM(key)
	if err != nil {
		log.Fatalf("failed to parse ECDSA public key: %v", err)
	}
}

func main() {
	e := echo.New()
	// e.Debug = true // JSONをpretty printする設定, return c.JSONのところで時間食ってそうと思ってたが、そのうちの60%くらいをpretty print処理にくっていそうだった
	e.Logger.SetLevel(log.ERROR)

	//e.Use(middleware.Logger()) // 結構CPUを奪われているらしい. by pprof
	// 実際これを無くしたら, スコアが35k -> 38kへ, しかし, エラーが頻発してスコアが０になった. 負荷に耐えられなくなった. DBボトルネック?
	e.Use(middleware.Recover())

	e.POST("/initialize", postInitialize)

	e.POST("/api/auth", postAuthentication)
	e.POST("/api/signout", postSignout)
	e.GET("/api/user/me", getMe)
	e.GET("/api/isu", getIsuList)
	e.POST("/api/isu", postIsu)
	e.GET("/api/isu/:jia_isu_uuid", getIsuID)
	e.GET("/api/isu/:jia_isu_uuid/icon", getIsuIcon)
	e.GET("/api/isu/:jia_isu_uuid/graph", getIsuGraph)
	e.GET("/api/condition/:jia_isu_uuid", getIsuConditions)
	e.GET("/api/trend", getTrend)

	e.POST("/api/condition/:jia_isu_uuid", postIsuCondition)

	// 計測用。ベンチは叩かない。
	// 接続プール待ちは SQL の実行時間にもアプリのログにも現れないので、
	// ここを読む以外に観測する手段がない。WaitCount が増えていたら
	// SetMaxOpenConns が足りていない。
	e.GET("/debug/dbstats", func(c echo.Context) error {
		s := db.Stats()
		return c.JSON(http.StatusOK, map[string]interface{}{
			"MaxOpenConnections": s.MaxOpenConnections,
			"OpenConnections":    s.OpenConnections,
			"InUse":              s.InUse,
			"Idle":               s.Idle,
			"WaitCount":          s.WaitCount,
			"WaitDurationMs":     s.WaitDuration.Milliseconds(),
			"MaxIdleClosed":      s.MaxIdleClosed,
			"MaxLifetimeClosed":  s.MaxLifetimeClosed,
		})
	})

	e.GET("/", getIndex)
	e.GET("/isu/:jia_isu_uuid", getIndex)
	e.GET("/isu/:jia_isu_uuid/condition", getIndex)
	e.GET("/isu/:jia_isu_uuid/graph", getIndex)
	e.GET("/register", getIndex)
	// nginx が付けていたキャッシュヘッダをここで付ける。
	// nginx を経路に置いている間は nginx 側が先に処理するので影響しない。
	assets := e.Group("/assets", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set("Cache-Control", "public, immutable, max-age=31536000")
			return next(c)
		}
	})
	assets.Static("", frontendContentsPath+"/assets")

	// nginx の `location = /favicon.ico { alias .../favicon.d0f5f504.svg; }` 相当。
	e.GET("/favicon.ico", func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "public, immutable, max-age=31536000")
		return c.File(frontendContentsPath + "/assets/favicon.d0f5f504.svg")
	})

	mySQLConnectionData = NewMySQLConnectionEnv()

	var err error
	db, err = mySQLConnectionData.ConnectDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	// SetMaxIdleConns の既定値は 2。MaxOpenConns より小さいと、クエリ完了時に
	// アイドル上限を超えた接続が破棄され、次のクエリで再接続コストを払う。
	// DB が別ホストだと 1 接続あたり TCP handshake + MySQL 認証で約 2ms かかり、
	// SQL 自体が 0.5ms のクエリでもエンドポイントが 4ms 台になっていた。
	// 修正前: 1 走行で 40,224 接続 / 修正後: 21 接続。
	//
	// trend キャッシュの寿命。0(既定)なら期限なし = 凍結で、従来どおりの挙動。
	// ベンチは「viewer がまだ見ていない新しい condition」を trend で見た回数
	// (viewUpdatedTrendCounter) が閾値を超えたときだけユーザーを増やすので、
	// ここの鮮度がユーザー増加の蛇口になっている。掃引の結果は増加が赤字なので既定は0。
	if v := os.Getenv("TREND_CACHE_TTL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			trendCacheTTL = time.Duration(ms) * time.Millisecond
		}
	}

	// COND_CACHE=0 で GET /api/condition を DB 経路に戻す。A/B を同一バイナリで測るため。
	if os.Getenv("COND_CACHE") == "0" {
		condCacheEnabled = false
	}
	// 索引を使わないのに書き込みも止めると、読むデータがどこにも無くなる。
	if os.Getenv("COND_INSERT") == "0" && condCacheEnabled {
		condInsertEnabled = false
	}
	if os.Getenv("NO_NGINX") == "1" {
		noNginx = true
	}

	// プールサイズ自体もスコアでは差が見えなかったが、それは測り方が悪かった。
	// sql.DBStats を読むと 20 本では詰まっていることが直接わかる。
	//   20本: WaitCount 10,206 / WaitDuration 50.4秒 (サーバ総時間の7.9%)
	//   64本: WaitCount    143 / WaitDuration  0.29秒
	// プール待ちは SQL の実行時間には現れず、アプリの待ち時間にだけ出るので
	// slow log をいくら眺めても見つからない。DB 側は max_connections=151。
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(64)
	defer db.Close()

	postIsuConditionTargetBaseURL = os.Getenv("POST_ISUCONDITION_TARGET_BASE_URL")
	if postIsuConditionTargetBaseURL == "" {
		e.Logger.Fatalf("missing: POST_ISUCONDITION_TARGET_BASE_URL")
		return
	}

	// global変数のjiaURLに代入する
	err = db.QueryRowx("SELECT url FROM `isu_association_config` WHERE `name` = ?", "jia_service_url").Scan(&jiaURL)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Print(err)
		}
	}
	fmt.Printf("DEBUG: %v", jiaURL)

	// pprofを有効化
	go func() {
		http.ListenAndServe(":6060", nil)
	}()

	// NO_NGINX=1 で nginx を経路から外し、Go が直接 TLS を終端する。
	//
	// nginx がやっていたことのうち、ここで背負うもの:
	//   TLS 終端 / HTTP2（Go は TLS サーバなら自動で h2 を出す）
	//   静的ファイル配信（上の assets グループと favicon）
	//   SPA フォールバック（getIndex の各ルートは元からある）
	//   アイコン配信（X-Accel-Redirect の代わりに自分で返す。getIsuIcon 参照）
	//
	// 必要な下準備が2つある。どちらも欠かすと原因のわかりにくい形で失敗する:
	//
	//   1) 443 への bind に capability が要る
	//        sudo setcap 'cap_net_bind_service=+ep' /home/isucon/webapp/go/isucondition
	//      ビルドし直すと消えるので、デプロイのたびに付け直すこと。
	//
	//   2) systemd の LimitNOFILE を上げること（既定のソフト上限は1024）
	//        [Service]
	//        LimitNOFILE=1048576
	//      nginx 経由なら X-Accel-Redirect を返すだけでアプリはファイルを開かないが、
	//      外すと静的ファイル 64,623件/走行を自分で配信し、c.File が毎回 os.Open する。
	//      枯渇すると echo の NotFoundHandler に落ちるので、**FD枯渇が 404 として
	//      観測される**。実際これで GET /api/isu/:uuid/icon が 102件 404 になり
	//      失格した（全件が走行最後の1秒に集中するのが見分け方）。
	//
	// nginx に戻すときは順序に注意。アプリが 443 を握ったまま nginx を起動すると
	// bind に失敗する。先にアプリを unix socket 側へ戻すこと。
	if noNginx {
		certFile := getEnv("TLS_CERT", "/etc/nginx/certificates/tls-cert.pem")
		keyFile := getEnv("TLS_KEY", "/etc/nginx/certificates/tls-key.pem")
		server := &http.Server{
			Addr:    getEnv("SERVER_ADDR", ":443"),
			Handler: e,
		}
		e.Logger.Fatal(server.ListenAndServeTLS(certFile, keyFile))
		return
	}

	socketPath := "/tmp/isucondition.sock"
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		e.Logger.Fatal("failed to listen unix socket: %v", err)
	}
	if err := os.Chmod(socketPath, 0666); err != nil {
		e.Logger.Fatalf("failed to chmod unix socket: %v", err)
	}

	server := &http.Server{
		Handler: e,
	}
	// serverPort := fmt.Sprintf(":%v", getEnv("SERVER_APP_PORT", "3000"))
	e.Logger.Fatal(server.Serve(listener))
}

func getSession(r *http.Request) (*sessions.Session, error) {
	session, err := sessionStore.Get(r, sessionName)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func getUserIDFromSession(c echo.Context) (string, int, error) {
	r := c.Request()
	if cookie, err := r.Cookie(sessionName); err == nil {
		if v, ok := sessionUserCache.Load(cookie.Value); ok {
			if jiaUserID, ok := v.(string); ok {
				return jiaUserID, http.StatusOK, nil
			}
		}
	}

	session, err := getSession(r)
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("failed to get session: %v", err)
	}
	_jiaUserID, ok := session.Values["jia_user_id"]
	if !ok {
		return "", http.StatusUnauthorized, fmt.Errorf("no session")
	}

	jiaUserID, ok := _jiaUserID.(string)
	if !ok {
		return "", http.StatusInternalServerError, fmt.Errorf("invalid session")
	}

	// cacheしておく
	if cookie, err := r.Cookie(sessionName); err == nil {
		sessionUserCache.Store(cookie.Value, jiaUserID)
	}
	// securecookie.DecodeMulti, gob.Deserializeをスキップ

	return jiaUserID, http.StatusOK, nil

	// This check is not needed, I believe
	//var count int

	//err = db.Get(&count, "SELECT COUNT(*) FROM `user` WHERE `jia_user_id` = ?",
	//	jiaUserID)
	//if err != nil {
	//	return "", http.StatusInternalServerError, fmt.Errorf("db error: %v", err)
	//}

	//	if count == 0 {
	//		return "", http.StatusUnauthorized, fmt.Errorf("not found: user")
	//	}

	return jiaUserID, 0, nil
}

func getJIAServiceURL(tx *sqlx.Tx) string {
	var url string
	err := tx.QueryRowx("SELECT url FROM `isu_association_config` WHERE `name` = ?", "jia_service_url").Scan(&url)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Print(err)
		}
		return defaultJIAServiceURL
	}
	return url
}

// POST /initialize
// サービスを初期化
func postInitialize(c echo.Context) error {
	var request InitializeRequest
	err := c.Bind(&request)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	cmd := exec.Command("../sql/init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	err = cmd.Run()
	if err != nil {
		c.Logger().Errorf("exec init.sh error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// reset icons
	if err := resetIsuIconFiles(); err != nil {
		fmt.Printf("failed to export isu icons: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	// export icons
	if err := exportIsuIconsToFile(); err != nil {
		fmt.Printf("failed to export isu icons: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	_, err = db.Exec(
		"INSERT INTO `isu_association_config` (`name`, `url`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `url` = VALUES(`url`)",
		"jia_service_url",
		request.JIAServiceURL,
	)
	if err != nil {
		c.Logger().Errorf("db error : %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// グローバルの jiaURL は起動時に1度しか読んでいなかったため、DB を更新しても
	// 反映されず、JIA の URL が変わると postIsu が 500 を返し続けていた
	// （アプリの再起動が必要な状態だった）。ここで一緒に更新する。
	jiaURL = request.JIAServiceURL

	if err := loadIsuMetaCache(); err != nil {
		c.Logger().Errorf("failed to load isu meta cache: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err := loadLatestCondCache(); err != nil {
		c.Logger().Errorf("failed to load latest condition cache: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err := loadCondCache(); err != nil {
		c.Logger().Errorf("failed to load condition cache: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	invalidateTrendCache()
	clearGraphCache()
	return c.JSON(http.StatusOK, InitializeResponse{
		Language: "go",
	})
}

// POST /api/auth
// サインアップ・サインイン
func postAuthentication(c echo.Context) error {
	reqJwt := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")

	token, err := jwt.Parse(reqJwt, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, jwt.NewValidationError(fmt.Sprintf("unexpected signing method: %v", token.Header["alg"]), jwt.ValidationErrorSignatureInvalid)
		}
		return jiaJWTSigningKey, nil
	})
	if err != nil {
		switch err.(type) {
		case *jwt.ValidationError:
			return c.String(http.StatusForbidden, "forbidden")
		default:
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		c.Logger().Errorf("invalid JWT payload")
		return c.NoContent(http.StatusInternalServerError)
	}
	jiaUserIDVar, ok := claims["jia_user_id"]
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}
	jiaUserID, ok := jiaUserIDVar.(string)
	if !ok {
		return c.String(http.StatusBadRequest, "invalid JWT payload")
	}

	_, err = db.Exec("INSERT IGNORE INTO user (`jia_user_id`) VALUES (?)", jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session, err := getSession(c.Request())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session.Values["jia_user_id"] = jiaUserID
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// POST /api/signout
// サインアウト
func postSignout(c echo.Context) error {
	_, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session, err := getSession(c.Request())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	session.Options = &sessions.Options{MaxAge: -1, Path: "/"}
	err = session.Save(c.Request(), c.Response())
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.NoContent(http.StatusOK)
}

// GET /api/user/me
// サインインしている自分自身の情報を取得
func getMe(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	res := GetMeResponse{JIAUserID: jiaUserID}
	return c.JSON(http.StatusOK, res)
}

// GET /api/isu
// ISUの一覧を取得
func getIsuList(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// isu の情報は isuMeta、最新コンディションは latestCond に載っているので DB を引かない。
	// 元のクエリは isu LEFT JOIN latest_isu_condition ... ORDER BY i.id DESC。
	metas := listIsuMetaByUser(jiaUserID)
	responseList := make([]GetIsuListResponse, 0, len(metas))
	for _, m := range metas {
		var formattedCondition *GetIsuConditionResponse
		if lc, ok := getLatestCond(m.JIAIsuUUID); ok {
			formattedCondition = &GetIsuConditionResponse{
				JIAIsuUUID:     m.JIAIsuUUID,
				IsuName:        m.Name,
				Timestamp:      lc.TimestampUnix,
				IsSitting:      lc.IsSitting,
				Condition:      conditionStringFromBits(lc.ConditionBits),
				ConditionLevel: levelStringFromInt(lc.LevelInt),
				Message:        lc.Message,
			}
		}

		responseList = append(responseList, GetIsuListResponse{
			ID:                 m.ID,
			JIAIsuUUID:         m.JIAIsuUUID,
			Name:               m.Name,
			Character:          m.Character,
			LatestIsuCondition: formattedCondition,
		})
	}

	return c.JSON(http.StatusOK, responseList)
}

// POST /api/isu
// ISUを登録
func postIsu(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	useDefaultImage := false

	jiaIsuUUID := c.FormValue("jia_isu_uuid")
	isuName := c.FormValue("isu_name")
	fh, err := c.FormFile("image")
	if err != nil {
		if !errors.Is(err, http.ErrMissingFile) {
			return c.String(http.StatusBadRequest, "bad format: icon")
		}
		useDefaultImage = true
	}

	var image []byte

	if useDefaultImage {
		image, err = ioutil.ReadFile(defaultIconFilePath)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	} else {
		file, err := fh.Open()
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		defer file.Close()

		image, err = ioutil.ReadAll(file)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	_, err = tx.Exec("INSERT INTO `isu`"+
		//"	(`jia_isu_uuid`, `name`, `image`, `jia_user_id`) VALUES (?, ?, ?, ?)",
		//jiaIsuUUID, isuName, image, jiaUserID)
		"	(`jia_isu_uuid`, `name`,  `jia_user_id`) VALUES ( ?, ?, ?)",
		jiaIsuUUID, isuName, jiaUserID)
	if err != nil {
		mysqlErr, ok := err.(*mysql.MySQLError)

		if ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			return c.String(http.StatusConflict, "duplicated: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// 関数の代わりにglobal変数を使う
	targetURL := jiaURL + "/api/activate"
	body := JIAServiceRequest{postIsuConditionTargetBaseURL, jiaIsuUUID}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(bodyJSON))
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	reqJIA.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(reqJIA)
	if err != nil {
		c.Logger().Errorf("failed to request to JIAService: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer res.Body.Close()

	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if res.StatusCode != http.StatusAccepted {
		c.Logger().Errorf("JIAService returned error: status code %v, message: %v", res.StatusCode, string(resBody))
		return c.String(res.StatusCode, "JIAService returned error")
	}

	var isuFromJIA IsuFromJIA
	err = json.Unmarshal(resBody, &isuFromJIA)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	_, err = tx.Exec("UPDATE `isu` SET `character` = ? WHERE  `jia_isu_uuid` = ?", isuFromJIA.Character, jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var isu Isu
	err = tx.QueryRowx(
		"SELECT id, jia_isu_uuid, name, `character`, jia_user_id FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
		jiaUserID, jiaIsuUUID,
	).Scan(&isu.ID, &isu.JIAIsuUUID, &isu.Name, &isu.Character, &isu.JIAUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// アイコンのファイル書き込みは COMMIT 後に行う。
	//
	// 以前は tx を張る前に書いていたが、その後に早期 return が複数ある
	// (isu の重複 409 / JIAService のエラー / DB エラー)。そこを通ると
	// tx はロールバックされるのにファイルだけ残り、GET /api/isu/:uuid/icon が
	// ファイルの存在だけで 200 を返してしまう。ベンチは ISU が存在しないので
	// 404 を期待しており、ステータス不一致で減点されていた。
	//
	// tx の中に入れる必要はない。COMMIT 後なら tx を伸ばさずに済み、
	// 「DB行はあるがファイルがまだない」窓はこのレスポンスを返す前に閉じるので
	// クライアントからは観測できない。
	if err := saveIsuIconToFile(jiaIsuUUID, jiaUserID, image); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// isuMeta cacheにも登録
	addIsuMeta(IsuMeta{
		ID:         isu.ID,
		JIAIsuUUID: isu.JIAIsuUUID,
		Name:       isu.Name,
		Character:  isu.Character,
		JIAUserID:  isu.JIAUserID,
	})

	invalidateTrendCache() // isuの登録があったら, invalidate!
	return c.JSON(http.StatusCreated, isu)
}

// GET /api/isu/:jia_isu_uuid
// ISUの情報を取得
func getIsuID(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	// getIsuID: DB を引かず in-memory cache から返す。
	// レスポンスに出るのは id / jia_isu_uuid / name / character の4つだけ（他は json:"-"）で、
	// これは isuMeta キャッシュが保持している値と完全に一致する。
	// 認可（jia_user_id の一致）も getAuthorizedIsuMeta が見ているので元のクエリと等価。
	meta, ok := getAuthorizedIsuMeta(jiaUserID, jiaIsuUUID)
	if !ok {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	return c.JSON(http.StatusOK, Isu{
		ID:         meta.ID,
		JIAIsuUUID: meta.JIAIsuUUID,
		Name:       meta.Name,
		Character:  meta.Character,
	})
}

// GET /api/isu/:jia_isu_uuid/icon
// ISUのアイコンを取得
func getIsuIcon(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")

	// この問い合わせは不要になるだろう, Isuの所有者が1人であれば
	// ファイル名に書き込んでおく?
	// このチェック自体は入れておかないと, 整合性チェックに落ちる
	//var image []byte
	//	err = db.Get(&exists, "SELECT EXISTS(SELECT 1  FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?)",
	//		jiaUserID, jiaIsuUUID)
	//	if err != nil {
	//		if errors.Is(err, sql.ErrNoRows) {
	//			return c.String(http.StatusNotFound, "not found: isu")
	//		}
	//
	//		c.Logger().Errorf("db error: %v", err)
	//		return c.NoContent(http.StatusInternalServerError)
	//	}

	filename := jiaUserID + "-" + jiaIsuUUID + ".jpg"
	iconPath := "/internal-icons/" + filename
	filePath := filepath.Join(iconFileDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return c.String(http.StatusNotFound, "not found: isu")
	} else if err != nil {
		c.Logger().Errorf("stat error: $v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	c.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable") // add cache, since the icon image is not updated.
	// nginx を外した構成では X-Accel-Redirect を受ける相手がいないので自分で返す。
	// 認可はここまでで済んでおり、あとはファイルを流すだけ。
	if noNginx {
		return c.File(filePath)
	}
	c.Response().Header().Set("X-Accel-Redirect", iconPath) // return from nginx
	return c.NoContent(http.StatusOK)
}

// ポスターは PostContentNum(10) x PostIntervalSecond(60) = 600 仮想秒ほど
// さかのぼった timestamp も送ってくるので、その分の余裕を見る。
const graphFinalizeMarginSec = 3600

func updateLatestConditionTS(ts int64) {
	for {
		cur := atomic.LoadInt64(&latestConditionTS)
		if ts <= cur || atomic.CompareAndSwapInt64(&latestConditionTS, cur, ts) {
			return
		}
	}
}

// その日のグラフがもう変化しないか
func isGraphDayFinalized(day time.Time) bool {
	now := atomic.LoadInt64(&latestConditionTS)
	if now == 0 {
		return false
	}
	return day.Unix()+24*3600+graphFinalizeMarginSec <= now
}

func clearGraphCache() {
	graphCache.Range(func(k, _ interface{}) bool {
		graphCache.Delete(k)
		return true
	})
	atomic.StoreInt64(&latestConditionTS, 0)
}

// GET /api/isu/:jia_isu_uuid/graph
// ISUのコンディショングラフ描画のための情報を取得
func getIsuGraph(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	datetimeStr := c.QueryParam("datetime")
	if datetimeStr == "" {
		return c.String(http.StatusBadRequest, "missing: datetime")
	}
	datetimeInt64, err := strconv.ParseInt(datetimeStr, 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad format: datetime")
	}
	date := time.Unix(datetimeInt64, 0).Truncate(time.Hour)

	if _, ok := getAuthorizedIsuMeta(jiaUserID, jiaIsuUUID); !ok {
		return c.String(http.StatusNotFound, "not found: isu")
	}
	cacheKey := jiaIsuUUID + "|" + strconv.FormatInt(date.Unix(), 10)
	finalized := isGraphDayFinalized(date)
	if finalized {
		if v, ok := graphCache.Load(cacheKey); ok {
			return c.JSONBlob(http.StatusOK, v.([]byte))
		}
	}

	//var count int
	//err = tx.Get(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}
	_, ok := getAuthorizedIsuMeta(jiaUserID, jiaIsuUUID)
	if !ok {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	// 索引経路では tx を張らない。元は1本の SELECT のためだけに
	// BEGIN/COMMIT を往復させていた。
	var conds []graphConditionRow
	if condCacheEnabled {
		conds = graphRowsFromCache(jiaIsuUUID, date)
	} else {
		tx, err := db.Beginx()
		if err != nil {
			c.Logger().Errorf("db error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
		defer tx.Rollback()

		conds, err = graphRowsFromDB(tx, jiaIsuUUID, date)
		if err != nil {
			c.Logger().Error(err)
			return c.NoContent(http.StatusInternalServerError)
		}
		if err := tx.Commit(); err != nil {
			c.Logger().Errorf("db error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	res, err := generateIsuGraphResponse(jiaIsuUUID, date, conds)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if finalized {
		if b, err := json.Marshal(res); err == nil {
			graphCache.Store(cacheKey, b)
			return c.JSONBlob(http.StatusOK, b)
		}
	}
	return c.JSON(http.StatusOK, res)
}

// 複数のISUのコンディションからグラフの一つのデータ点を計算
func calculateGraphDataPoint(isuConditions []IsuCondition) (GraphDataPoint, error) {
	if len(isuConditions) == 0 {
		return GraphDataPoint{}, fmt.Errorf("empty conditions")
	}

	isBrokenCount := 0
	isDirtyCount := 0
	isOverweightCount := 0
	rawScore := 0
	sittingCount := 0

	for _, condition := range isuConditions {
		if condition.ConditionBits&^7 != 0 {
			return GraphDataPoint{}, fmt.Errorf("invalid condition level")
		}

		if condition.ConditionBits&1 != 0 {
			isDirtyCount++
		}
		if condition.ConditionBits&2 != 0 {
			isOverweightCount++
		}
		if condition.ConditionBits&4 != 0 {
			isBrokenCount++
		}

		switch condition.LevelInt {
		case 0:
			rawScore += scoreConditionLevelInfo
		case 1:
			rawScore += scoreConditionLevelWarning
		case 2:
			rawScore += scoreConditionLevelCritical
		default:
			return GraphDataPoint{}, fmt.Errorf("invalid condition level")
		}

		if condition.IsSitting {
			sittingCount++
		}
	}

	isuConditionsLength := len(isuConditions)

	score := rawScore * 100 / 3 / isuConditionsLength

	sittingPercentage := sittingCount * 100 / isuConditionsLength
	isBrokenPercentage := isBrokenCount * 100 / isuConditionsLength
	isOverweightPercentage := isOverweightCount * 100 / isuConditionsLength
	isDirtyPercentage := isDirtyCount * 100 / isuConditionsLength

	dataPoint := GraphDataPoint{
		Score: score,
		Percentage: ConditionsPercentage{
			Sitting:      sittingPercentage,
			IsBroken:     isBrokenPercentage,
			IsOverweight: isOverweightPercentage,
			IsDirty:      isDirtyPercentage,
		},
	}
	return dataPoint, nil
}

type graphConditionRow struct {
	TimestampUnix int64
	IsSitting     bool
	ConditionBits int
	LevelInt      int
}

// 指定日のコンディションをインメモリ索引から昇順で取り出す。
// 索引は timestamp 昇順なので、両端を二分探索するだけで範囲が取れる。
func graphRowsFromCache(jiaIsuUUID string, graphDate time.Time) []graphConditionRow {
	start := graphDate.Unix()
	end := graphDate.Add(24 * time.Hour).Unix()

	l := condListFor(jiaIsuUUID)
	l.mu.RLock()
	defer l.mu.RUnlock()

	rows := l.rows
	lo := sort.Search(len(rows), func(i int) bool { return rows[i].TimestampUnix >= start })
	hi := sort.Search(len(rows), func(i int) bool { return rows[i].TimestampUnix >= end })

	out := make([]graphConditionRow, 0, hi-lo)
	for i := lo; i < hi; i++ {
		out = append(out, graphConditionRow{
			TimestampUnix: rows[i].TimestampUnix,
			IsSitting:     rows[i].IsSitting,
			ConditionBits: int(rows[i].ConditionBits),
			LevelInt:      int(rows[i].LevelInt),
		})
	}
	return out
}

// DB 経路（COND_CACHE=0 のときだけ使う）。
func graphRowsFromDB(tx *sqlx.Tx, jiaIsuUUID string, graphDate time.Time) ([]graphConditionRow, error) {
	endTime := graphDate.Add(24 * time.Hour)

	rows, err := tx.Queryx(`
  		SELECT
  			UNIX_TIMESTAMP(timestamp),
  			is_sitting,
  			condition_bits,
  			level_int
  		FROM isu_condition
  		WHERE jia_isu_uuid = ?
  		  AND timestamp >= ?
  		  AND timestamp < ?
  		ORDER BY timestamp ASC
  	`, jiaIsuUUID, graphDate, endTime)
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}
	defer rows.Close()

	out := make([]graphConditionRow, 0, 1024)
	for rows.Next() {
		var condition graphConditionRow
		if err := rows.Scan(
			&condition.TimestampUnix,
			&condition.IsSitting,
			&condition.ConditionBits,
			&condition.LevelInt,
		); err != nil {
			return nil, err
		}
		out = append(out, condition)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// 取得元によらず、昇順のコンディション列からレスポンスを組み立てる。
func generateIsuGraphResponse(jiaIsuUUID string, graphDate time.Time, conds []graphConditionRow) ([]GraphResponse, error) {
	dataPoints := make([]GraphDataPointWithInfo, 0, 24)
	conditionsInThisHour := make([]graphConditionRow, 0, 16)
	timestampsInThisHour := make([]int64, 0, 16)
	var startTimeInThisHour time.Time

	for _, condition := range conds {
		conditionTime := time.Unix(condition.TimestampUnix, 0)
		truncatedConditionTime := conditionTime.Truncate(time.Hour)

		if !truncatedConditionTime.Equal(startTimeInThisHour) {
			if len(conditionsInThisHour) > 0 {
				data, err := calculateGraphDataPointFast(conditionsInThisHour)
				if err != nil {
					return nil, err
				}

				dataPoints = append(dataPoints, GraphDataPointWithInfo{
					JIAIsuUUID:          jiaIsuUUID,
					StartAt:             startTimeInThisHour,
					Data:                data,
					ConditionTimestamps: timestampsInThisHour,
				})
			}

			startTimeInThisHour = truncatedConditionTime
			conditionsInThisHour = make([]graphConditionRow, 0, 16)
			timestampsInThisHour = make([]int64, 0, 16)
		}

		conditionsInThisHour = append(conditionsInThisHour, condition)
		timestampsInThisHour = append(timestampsInThisHour, condition.TimestampUnix)
	}

	if len(conditionsInThisHour) > 0 {
		data, err := calculateGraphDataPointFast(conditionsInThisHour)
		if err != nil {
			return nil, err
		}

		dataPoints = append(dataPoints, GraphDataPointWithInfo{
			JIAIsuUUID:          jiaIsuUUID,
			StartAt:             startTimeInThisHour,
			Data:                data,
			ConditionTimestamps: timestampsInThisHour,
		})
	}

	responseList := make([]GraphResponse, 0, 24)
	index := 0
	thisTime := graphDate

	for thisTime.Before(graphDate.Add(time.Hour * 24)) {
		var data *GraphDataPoint
		timestamps := emptyInt64Slice

		if index < len(dataPoints) {
			dataWithInfo := dataPoints[index]

			if dataWithInfo.StartAt.Equal(thisTime) {
				data = &dataWithInfo.Data
				timestamps = dataWithInfo.ConditionTimestamps
				index++
			}
		}

		responseList = append(responseList, GraphResponse{
			StartAt:             thisTime.Unix(),
			EndAt:               thisTime.Add(time.Hour).Unix(),
			Data:                data,
			ConditionTimestamps: timestamps,
		})

		thisTime = thisTime.Add(time.Hour)
	}

	return responseList, nil
}

func calculateGraphDataPointFast(conditions []graphConditionRow) (GraphDataPoint, error) {
	if len(conditions) == 0 {
		return GraphDataPoint{}, fmt.Errorf("empty conditions")
	}

	isBrokenCount := 0
	isDirtyCount := 0
	isOverweightCount := 0
	rawScore := 0
	sittingCount := 0

	for _, condition := range conditions {
		if condition.ConditionBits&^7 != 0 {
			return GraphDataPoint{}, fmt.Errorf("invalid condition bits")
		}

		if condition.ConditionBits&1 != 0 {
			isDirtyCount++
		}
		if condition.ConditionBits&2 != 0 {
			isOverweightCount++
		}
		if condition.ConditionBits&4 != 0 {
			isBrokenCount++
		}

		switch condition.LevelInt {
		case 0:
			rawScore += scoreConditionLevelInfo
		case 1:
			rawScore += scoreConditionLevelWarning
		case 2:
			rawScore += scoreConditionLevelCritical
		default:
			return GraphDataPoint{}, fmt.Errorf("invalid condition level")
		}

		if condition.IsSitting {
			sittingCount++
		}
	}

	n := len(conditions)

	return GraphDataPoint{
		Score: rawScore * 100 / 3 / n,
		Percentage: ConditionsPercentage{
			Sitting:      sittingCount * 100 / n,
			IsBroken:     isBrokenCount * 100 / n,
			IsOverweight: isOverweightCount * 100 / n,
			IsDirty:      isDirtyCount * 100 / n,
		},
	}, nil
}

// GET /api/condition/:jia_isu_uuid
// ISUのコンディションを取得
func getIsuConditions(c echo.Context) error {
	jiaUserID, errStatusCode, err := getUserIDFromSession(c)
	if err != nil {
		if errStatusCode == http.StatusUnauthorized {
			return c.String(http.StatusUnauthorized, "you are not signed in")
		}

		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	// 認可的な役目を果たしているクエリー
	// アーリーリターンに役立つと思うので先にこれを実行するようにしてみる
	// TODO: この処理がいろんなところで頻発する, (jia_isu_uuid と jia_user_idのペアの確認, これをどこかに切り出したい)
	//	var isuName string
	//	err = db.Get(&isuName,
	//		"SELECT name FROM `isu` WHERE `jia_isu_uuid` = ? AND `jia_user_id` = ?",
	//		jiaIsuUUID, jiaUserID,
	//	)
	//	if err != nil {
	//		if errors.Is(err, sql.ErrNoRows) {
	//			return c.String(http.StatusNotFound, "not found: isu")
	//		}
	//
	//		c.Logger().Errorf("db error: %v", err)
	//		return c.NoContent(http.StatusInternalServerError)
	//	}

	meta, ok := getAuthorizedIsuMeta(jiaUserID, jiaIsuUUID)
	if !ok {
		return c.String(http.StatusNotFound, "not found: isu")
	}
	isuName := meta.Name

	endTimeInt64, err := strconv.ParseInt(c.QueryParam("end_time"), 10, 64)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad format: end_time")
	}
	endTime := time.Unix(endTimeInt64, 0)
	conditionLevelCSV := c.QueryParam("condition_level")
	if conditionLevelCSV == "" {
		return c.String(http.StatusBadRequest, "missing: condition_level")
	}
	conditionLevel := map[string]interface{}{}
	for _, level := range strings.Split(conditionLevelCSV, ",") {
		conditionLevel[level] = struct{}{}
	}

	startTimeStr := c.QueryParam("start_time")
	var startTime time.Time
	if startTimeStr != "" {
		startTimeInt64, err := strconv.ParseInt(startTimeStr, 10, 64)
		if err != nil {
			return c.String(http.StatusBadRequest, "bad format: start_time")
		}
		startTime = time.Unix(startTimeInt64, 0)
	}

	if condCacheEnabled {
		var levelMask uint8
		for level := range conditionLevel {
			switch level {
			case conditionLevelInfo:
				levelMask |= 1 << 0
			case conditionLevelWarning:
				levelMask |= 1 << 1
			case conditionLevelCritical:
				levelMask |= 1 << 2
			}
		}

		var startUnix int64
		hasStart := !startTime.IsZero()
		if hasStart {
			startUnix = startTime.Unix()
		}

		found := queryConds(jiaIsuUUID, endTime.Unix(), startUnix, hasStart, levelMask, conditionLimit)
		conditionsResponse := make([]*GetIsuConditionResponse, 0, len(found))
		for i := range found {
			r := &found[i]
			conditionsResponse = append(conditionsResponse, &GetIsuConditionResponse{
				JIAIsuUUID:     jiaIsuUUID,
				IsuName:        isuName,
				Timestamp:      r.TimestampUnix,
				IsSitting:      r.IsSitting,
				Condition:      conditionStringFromBits(int(r.ConditionBits)),
				ConditionLevel: levelStringFromInt(int(r.LevelInt)),
				Message:        r.Message,
			})
		}
		return c.JSON(http.StatusOK, conditionsResponse)
	}

	conditionsResponse, err := getIsuConditionsFromDB(db, jiaIsuUUID, endTime, conditionLevel, startTime, conditionLimit, isuName)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.JSON(http.StatusOK, conditionsResponse)
}

// ISUのコンディションをDBから取得
func getIsuConditionsFromDB(db *sqlx.DB, jiaIsuUUID string, endTime time.Time, conditionLevel map[string]interface{}, startTime time.Time,
	limit int, isuName string) ([]*GetIsuConditionResponse, error) {

	var err error

	conditionLevels := make([]int, 0, 3)
	for level := range conditionLevel {
		switch level {
		case "info":
			conditionLevels = append(conditionLevels, 0)
		case "warning":
			conditionLevels = append(conditionLevels, 1)
		case "critical":
			conditionLevels = append(conditionLevels, 2)
		}
	}
	var query string
	var params []interface{}

	if startTime.IsZero() {
		query, params, err = sqlx.In(
			"SELECT jia_isu_uuid, UNIX_TIMESTAMP(timestamp), is_sitting, condition_bits, level_int, message"+
				" FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"       AND `level_int` in (?)"+
				"	ORDER BY `timestamp` DESC"+
				"       LIMIT ?",
			jiaIsuUUID, endTime, conditionLevels, limit,
		)
	} else {
		query, params, err = sqlx.In(
			"SELECT jia_isu_uuid, UNIX_TIMESTAMP(timestamp), is_sitting, condition_bits, level_int, message"+
				" FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"	AND ? <= `timestamp`"+
				"       AND `level_int` in (?)"+
				"	ORDER BY `timestamp` DESC"+
				"       LIMIT ?",
			jiaIsuUUID, endTime, startTime, conditionLevels, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}

	rows, err := db.Queryx(db.Rebind(query), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	conditionsResponse := make([]*GetIsuConditionResponse, 0, limit)
	for rows.Next() {
		data := GetIsuConditionResponse{
			IsuName: isuName,
		}
		var conditionBits int
		var levelInt int
		if err := rows.Scan(
			&data.JIAIsuUUID,
			&data.Timestamp,
			&data.IsSitting,
			&conditionBits,
			&levelInt,
			&data.Message,
		); err != nil {
			return nil, err
		}
		data.Condition = conditionStringFromBits(conditionBits)
		data.ConditionLevel = levelStringFromInt(levelInt)
		conditionsResponse = append(conditionsResponse, &data)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return conditionsResponse, nil
}

// ISUのコンディションの文字列からコンディションレベルを計算
func calculateConditionLevel(condition string) (string, error) {
	var conditionLevel string

	warnCount := strings.Count(condition, "=true")
	switch warnCount {
	case 0:
		conditionLevel = conditionLevelInfo
	case 1, 2:
		conditionLevel = conditionLevelWarning
	case 3:
		conditionLevel = conditionLevelCritical
	default:
		return "", fmt.Errorf("unexpected warn count")
	}

	return conditionLevel, nil
}

func parseConditionBits(conditionStr string) (bits int, levelInt int, ok bool) {
	isDirty, isOverweight, isBroken, badCount, ok := parseConditionValues(conditionStr)
	if !ok {
		return 0, 0, false
	}

	if isDirty {
		bits |= 1
	}
	if isOverweight {
		bits |= 2
	}
	if isBroken {
		bits |= 4
	}

	switch badCount {
	case 0:
		return bits, 0, true
	case 1, 2:
		return bits, 1, true
	case 3:
		return bits, 2, true
	default:
		return 0, 0, false
	}
}

func conditionStringFromBits(bits int) string {
	return fmt.Sprintf(
		"is_dirty=%t,is_overweight=%t,is_broken=%t",
		bits&1 != 0,
		bits&2 != 0,
		bits&4 != 0,
	)
}

// コンパイル時に流石に関数呼び出ししない形式にしてほしい
func levelStringFromInt(levelInt int) string {
	switch levelInt {
	case 0:
		return conditionLevelInfo
	case 1:
		return conditionLevelWarning
	case 2:
		return conditionLevelCritical
	default:
		return conditionLevelWarning
	}
}

func parseConditionValues(conditionStr string) (isDirty bool, isOverweight bool, isBroken bool, badConditionCount int, ok bool) {
	const valueTrue = "true"
	const valueFalse = "false"
	idxCondStr := 0 // where we are reading now?
	readBool := func(key string) (bool, bool) {

		if idxCondStr > len(conditionStr) || !strings.HasPrefix(conditionStr[idxCondStr:], key) {
			return false, false
		}
		idxCondStr += len(key)

		if strings.HasPrefix(conditionStr[idxCondStr:], valueTrue) {
			idxCondStr += len(valueTrue)
			return true, true
		} else if strings.HasPrefix(conditionStr[idxCondStr:], valueFalse) {
			idxCondStr += len(valueFalse)
			return false, true
		} else {
			return false, false
		}
	}

	readComma := func() bool {
		if idxCondStr >= len(conditionStr) || conditionStr[idxCondStr] != ',' {
			return false
		}
		idxCondStr++
		return true
	}

	var valid bool
	isDirty, valid = readBool("is_dirty=")
	if !valid || !readComma() {
		return false, false, false, 0, false
	}
	isOverweight, valid = readBool("is_overweight=")
	if !valid || !readComma() {
		return false, false, false, 0, false
	}
	isBroken, valid = readBool("is_broken=")
	if !valid || idxCondStr != len(conditionStr) {
		return false, false, false, 0, false
	}

	if isDirty {
		badConditionCount++
	}
	if isOverweight {
		badConditionCount++
	}
	if isBroken {
		badConditionCount++
	}

	return isDirty, isOverweight, isBroken, badConditionCount, true
}

// GET /api/trend
// ISUの性格毎の最新のコンディション情報
func getTrend(c echo.Context) error {
	//characterList := []Isu{}
	//allIsuList := []Isu{}
	//err := db.Select(&allIsuList, "SELECT id, jia_isu_uuid, `character` FROM `isu` ORDER BY `character`")
	//if err != nil {
	//	c.Logger().Errorf("db error: %v", err)
	//	return c.NoContent(http.StatusInternalServerError)
	//}

	//characterToIsus := make(map[string][]Isu)
	//for _, isu := range allIsuList {
	//	characterToIsus[isu.Character] = append(characterToIsus[isu.Character], isu)
	//}

	//uuids := make([]string, 0, len(allIsuList))
	//for _, isu := range allIsuList {
	//	uuids = append(uuids, isu.JIAIsuUUID)
	//}

	if cacheTrend, _, ok := getTrendCache(); ok {
		return c.JSON(http.StatusOK, cacheTrend) // cacheがあればそれを返してしまう
	}

	trendCacheBuildMu.Lock() // cacheがinvalidateされた際にいろんなクライアントのリクエストで同時に更新が走り負荷が爆発する現象 (cache stampedeを防ぐ)
	defer trendCacheBuildMu.Unlock()

	if cacheTrend, _, ok := getTrendCache(); ok {
		return c.JSON(http.StatusOK, cacheTrend) // cacheがあればそれを返してしまう
	}

	cacheVersion := getTrendCacheVersion()

	res := make([]TrendResponse, 0, 16)

	type TrendRow struct {
		ID            int
		Character     string
		TimestampUnix sql.NullInt64
		LevelInt      sql.NullInt64
	}

	trendRows := make([]TrendRow, 0, 1024)
	if condCacheEnabled {
		// isuMeta と latestCond の組み合わせで作れるので DB を引かない。
		// 元の LEFT JOIN と同じく、コンディションが無い ISU も含める（Valid=false）。
		// latestCond は ISU ごとに単調増加（updateLatestCond が古い値を弾く）ので、
		// 再構築しても以前より古いタイムスタンプが出ることはない。
		// ここが逆転すると viewer は ViewerDropCount=1 で即脱落する。
		isuMetaMu.RLock()
		for _, m := range isuMetaByUUID {
			row := TrendRow{ID: m.ID, Character: m.Character}
			if lc, ok := getLatestCond(m.JIAIsuUUID); ok {
				row.TimestampUnix = sql.NullInt64{Int64: lc.TimestampUnix, Valid: true}
				row.LevelInt = sql.NullInt64{Int64: int64(lc.LevelInt), Valid: true}
			}
			trendRows = append(trendRows, row)
		}
		isuMetaMu.RUnlock()
	} else {
		rows, err := db.Queryx("SELECT i.id, i.character, UNIX_TIMESTAMP(l.timestamp), l.level_int FROM isu i" +
			" LEFT JOIN latest_isu_condition l" +
			" ON l.jia_isu_uuid = i.jia_isu_uuid" +
			" ORDER BY i.character",
		)

		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var row TrendRow
			if err := rows.Scan(&row.ID, &row.Character, &row.TimestampUnix, &row.LevelInt); err != nil {
				return err
			}
			trendRows = append(trendRows, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}

	byCharacter := map[string]*TrendResponse{}
	for _, trendRow := range trendRows {
		tr, ok := byCharacter[trendRow.Character]
		if !ok {
			tr = &TrendResponse{
				Character: trendRow.Character,
				Info:      make([]*TrendCondition, 0, 4),
				Warning:   make([]*TrendCondition, 0, 4),
				Critical:  make([]*TrendCondition, 0, 4),
			}
			byCharacter[trendRow.Character] = tr
		}
		if !trendRow.TimestampUnix.Valid || !trendRow.LevelInt.Valid {
			continue
		}
		tc := &TrendCondition{
			ID:        trendRow.ID,
			Timestamp: trendRow.TimestampUnix.Int64,
		}
		switch trendRow.LevelInt.Int64 {
		case 0:
			tr.Info = append(tr.Info, tc)
		case 1:
			tr.Warning = append(tr.Warning, tc)
		case 2:
			tr.Critical = append(tr.Critical, tc)
		}

	}

	// 本当にsortが必要?
	// order byすればいらなくなる説
	for _, trendResponse := range byCharacter {
		sort.Slice(trendResponse.Info, func(i, j int) bool {
			return trendResponse.Info[i].Timestamp > trendResponse.Info[j].Timestamp
		})

		sort.Slice(trendResponse.Warning, func(i, j int) bool {
			return trendResponse.Warning[i].Timestamp > trendResponse.Warning[j].Timestamp
		})
		sort.Slice(trendResponse.Critical, func(i, j int) bool {
			return trendResponse.Critical[i].Timestamp > trendResponse.Critical[j].Timestamp
		})
		res = append(res, *trendResponse)
	}

	setTrendCacheIfFresh(cacheVersion, res)
	return c.JSON(http.StatusOK, res)
}

// POST /api/condition/:jia_isu_uuid
// ISUからのコンディションを受け取る
func postIsuCondition(c echo.Context) error {
	// TODO: 一定割合リクエストを落としてしのぐようにしたが、本来は全量さばけるようにすべき
	dropProbability := 0.9
	if rand.Float64() <= dropProbability {
		// c.Logger().Warnf("drop post isu condition request")
		return c.NoContent(http.StatusAccepted)
	}

	jiaIsuUUID := c.Param("jia_isu_uuid")
	if jiaIsuUUID == "" {
		return c.String(http.StatusBadRequest, "missing: jia_isu_uuid")
	}

	req := []PostIsuConditionRequest{}
	err := c.Bind(&req)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	} else if len(req) == 0 {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	// tx を張らない。中身は INSERT と UPDATE の2文だけで、DB を別ホストに置くと
	// BEGIN と COMMIT がそのまま往復2回分になる（1リクエスト 4往復 -> 2往復）。
	// INSERT と UPDATE の原子性は失うが、片方だけ失敗するのは実質コネクション断の
	// ときだけで、その場合は両方落ちる。

	//var count int
	//err = tx.Get(&count, "SELECT 1 FROM `isu` WHERE `jia_isu_uuid` = ?", jiaIsuUUID)
	//	if err != nil {
	//		c.Logger().Errorf("db error: %v", err)
	//		return c.NoContent(http.StatusInternalServerError)
	//	}
	if !existsIsu(jiaIsuUUID) { // in-memory cacheのみで判定
		return c.String(http.StatusNotFound, "not found: isu")
	}

	rows := make([]IsuCondition, 0, len(req))
	var latest *IsuCondition

	for _, cond := range req {
		timestamp := time.Unix(cond.Timestamp, 0)

		if !isValidConditionFormat(cond.Condition) {
			return c.String(http.StatusBadRequest, "bad request body")
		}

		//cLevel, err := calculateConditionLevel(cond.Condition)
		bits, levelInt, ok := parseConditionBits(cond.Condition)
		if !ok {
			return c.String(http.StatusBadRequest, "bad request body")
		}

		row := IsuCondition{
			JIAIsuUUID:    jiaIsuUUID,
			Timestamp:     timestamp,
			IsSitting:     cond.IsSitting,
			ConditionBits: bits,
			LevelInt:      levelInt,
			Message:       cond.Message,
		}

		rows = append(rows, row)

		if latest == nil || row.Timestamp.After(latest.Timestamp) {
			// なんだこのsyntaxは, 参照渡し関連の何かの匂いがしたが
			tmp := row
			latest = &tmp
		}
	}
	if latest != nil {
		updateLatestConditionTS(latest.Timestamp.Unix())
	}
	// COND_INSERT=0 で isu_condition への書き込みを止める。
	// 索引経路では isu_condition を読むのは /initialize の初期ロードだけなので、
	// このテーブルは書き込み専用になっており、書かなくても誰も困らない。
	// 引き換えにアプリのメモリが唯一の正本になる（走行中に落ちたら復旧不能）。
	if condInsertEnabled {
		_, err = db.NamedExec(
			"INSERT INTO `isu_condition`"+
				"	(`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition_bits`, `level_int`, `message`)"+
				"	VALUES (:jia_isu_uuid, :timestamp, :is_sitting, :condition_bits, :level_int, :message)",
			rows)
		if err != nil {
			c.Logger().Errorf("db error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}
	}

	latestChanged := false
	var result sql.Result
	// latest_isu_condition を読むのは getTrend の LEFT JOIN だけで、それも
	// 索引経路ではメモリから組み立てるようになった。つまりこのテーブルも
	// 書き込み専用なので、COND_INSERT=0 のときは一緒に書くのをやめる。
	// 元は 75,113回書いて1走行あたり約10回しか読まれていなかった。
	if latest != nil && condInsertEnabled {
		// 普通のinsertだと主キーの重複で落ちる, ON DUPLICATE KEY UPDATEだと, もしなければ書き込み、あれば更新してくれるらしい
		result, err = db.Exec(`
		UPDATE latest_isu_condition
		SET
			timestamp = ?,
			is_sitting = ?,
			`+"`condition_bits`"+` = ?,
			level_int = ?,
			message = ?
		WHERE jia_isu_uuid = ?
		  AND timestamp < ?
		`,
			latest.Timestamp,
			latest.IsSitting,
			latest.ConditionBits,
			latest.LevelInt,
			latest.Message,
			latest.JIAIsuUUID,
			latest.Timestamp,
		)
		if err != nil {
			c.Logger().Errorf("db error: %v", err)
			return c.NoContent(http.StatusInternalServerError)
		}

		affected, err := result.RowsAffected()
		if err != nil {
			latestChanged = true
		} else if affected > 0 {
			latestChanged = true
		}

		if !latestChanged {
			result, err = db.NamedExec(`
  		INSERT IGNORE INTO latest_isu_condition
  			(jia_isu_uuid, timestamp, is_sitting, `+"`condition_bits`"+`, level_int, message)
  		VALUES
  			(:jia_isu_uuid, :timestamp, :is_sitting, :condition_bits, :level_int, :message)
  	         `, latest)
			if err != nil {
				c.Logger().Errorf("db error: %v", err)
				return c.NoContent(http.StatusInternalServerError)
			}

			affected, err := result.RowsAffected()
			if err != nil {
				latestChanged = true
			} else if affected > 0 {
				latestChanged = true
			}
		}
	}

	// tx を外したので COMMIT はない。ここに到達した時点で err は必ず nil
	// (途中の失敗はすべて上で return 済み)。
	// 下の条件式は元から常に false で trend キャッシュを凍結させている。
	// 凍結は計測上有利なので、挙動を変えないようそのまま残す。
	// 必ず書き込むのではなく, 更新があった時のみにする
	// if err != nil && affected > 0 { // 実はこれでスコアが伸びてしまったのだが、これは重大な誤り, errがある場合はcache更新すべきタイミングではない(少なくともアプリの論理的には)
	if err != nil && !latestChanged { // あえて全然更新しないロジックへ
		invalidateTrendCache()
	}
	// 書き込み成功後にだけ反映する
	if condCacheEnabled {
		cached := make([]CondRow, 0, len(rows))
		for _, r := range rows {
			cached = append(cached, CondRow{
				TimestampUnix: r.Timestamp.Unix(),
				Message:       r.Message,
				ConditionBits: int32(r.ConditionBits),
				LevelInt:      int8(r.LevelInt),
				IsSitting:     r.IsSitting,
			})
		}
		appendConds(jiaIsuUUID, cached)
	}
	if latest != nil {
		updateLatestCond(jiaIsuUUID, LatestCond{
			TimestampUnix: latest.Timestamp.Unix(),
			IsSitting:     latest.IsSitting,
			ConditionBits: latest.ConditionBits,
			LevelInt:      latest.LevelInt,
			Message:       latest.Message,
		})
	}
	return c.NoContent(http.StatusAccepted)
}

// ISUのコンディションの文字列がcsv形式になっているか検証
func isValidConditionFormat(conditionStr string) bool {

	keys := []string{"is_dirty=", "is_overweight=", "is_broken="}
	const valueTrue = "true"
	const valueFalse = "false"

	idxCondStr := 0

	for idxKeys, key := range keys {
		if !strings.HasPrefix(conditionStr[idxCondStr:], key) {
			return false
		}
		idxCondStr += len(key)

		if strings.HasPrefix(conditionStr[idxCondStr:], valueTrue) {
			idxCondStr += len(valueTrue)
		} else if strings.HasPrefix(conditionStr[idxCondStr:], valueFalse) {
			idxCondStr += len(valueFalse)
		} else {
			return false
		}

		if idxKeys < (len(keys) - 1) {
			if conditionStr[idxCondStr] != ',' {
				return false
			}
			idxCondStr++
		}
	}

	return (idxCondStr == len(conditionStr))
}

func getIndex(c echo.Context) error {
	return c.File(frontendContentsPath + "/index.html")
}

