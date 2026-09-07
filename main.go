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

	_ "net/http/pprof"
	"path/filepath"
	"sync"
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
	iconFileDir                 = "../icons"
)

var (
	db                  *sqlx.DB
	sessionStore        sessions.Store
	mySQLConnectionData *MySQLConnectionEnv

	jiaJWTSigningKey *ecdsa.PublicKey

	postIsuConditionTargetBaseURL string // JIAへのactivate時に登録する，ISUがconditionを送る先のURL
	jiaServiceURL                 = defaultJIAServiceURL
)

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
	Condition  string    `db:"condition"`
	Level      string    `db:"level"`
	Message    string    `db:"message"`
	CreatedAt  time.Time `db:"created_at"`
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
		Host:     getEnv("MYSQL_HOST", "127.0.0.1"),
		Port:     getEnv("MYSQL_PORT", "3306"),
		User:     getEnv("MYSQL_USER", "isucon"),
		DBName:   getEnv("MYSQL_DBNAME", "isucondition"),
		Password: getEnv("MYSQL_PASS", "isucon"),
	}
}

func (mc *MySQLConnectionEnv) ConnectDB() (*sqlx.DB, error) {
	dsn := fmt.Sprintf("%v:%v@tcp(%v:%v)/%v?parseTime=true&loc=Asia%%2FTokyo", mc.User, mc.Password, mc.Host, mc.Port, mc.DBName)
	return sqlx.Open("mysql", dsn)
}

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

// Global変数を定義
type latestConditionCacheEntry struct {
	IsuID     int
	Character string
	Timestamp time.Time
	IsSitting bool
	Condition string
	Level     string
	Message   string
}

type LatestConditionCache struct {
	shards [latestConditionCacheShardCount]latestConditionCacheShard
}

type latestConditionCacheShard struct {
	mu sync.RWMutex
	m  map[string]latestConditionCacheEntry
}

const latestConditionCacheShardCount = 64

var latestConditionCache = newLatestConditionCache()

func newLatestConditionCache() LatestConditionCache {
	c := LatestConditionCache{}
	for i := range c.shards {
		c.shards[i].m = make(map[string]latestConditionCacheEntry)
	}
	return c
}

func (c *LatestConditionCache) shard(key string) *latestConditionCacheShard {
	var h uint32
	for i := 0; i < len(key); i++ {
		h = h*33 + uint32(key[i])
	}
	return &c.shards[h%latestConditionCacheShardCount]
}

func (c *LatestConditionCache) Get(key string) (latestConditionCacheEntry, bool) {
	s := c.shard(key)
	s.mu.RLock()
	v, ok := s.m[key]
	s.mu.RUnlock()
	return v, ok
}

func (c *LatestConditionCache) SetIfNewer(key string, entry latestConditionCacheEntry) {
	s := c.shard(key)
	s.mu.Lock()
	current, ok := s.m[key]
	if !ok || current.Timestamp.Before(entry.Timestamp) {
		s.m[key] = entry
	}
	s.mu.Unlock()
}

func (c *LatestConditionCache) ReplaceAll(next map[string]latestConditionCacheEntry) {
	sharded := make([]map[string]latestConditionCacheEntry, latestConditionCacheShardCount)
	for i := range sharded {
		sharded[i] = make(map[string]latestConditionCacheEntry)
	}
	for k, v := range next {
		var h uint32
		for i := 0; i < len(k); i++ {
			h = h*33 + uint32(k[i])
		}
		sharded[h%latestConditionCacheShardCount][k] = v
	}

	for i := range c.shards {
		c.shards[i].mu.Lock()
		c.shards[i].m = sharded[i]
		c.shards[i].mu.Unlock()
	}
}

func (c *LatestConditionCache) Snapshot() map[string]latestConditionCacheEntry {
	total := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.RLock()
		total += len(s.m)
		s.mu.RUnlock()
	}

	snapshot := make(map[string]latestConditionCacheEntry, total)
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.RLock()
		for k, v := range s.m {
			snapshot[k] = v
		}
		s.mu.RUnlock()
	}
	return snapshot
}

// jia_isu_uuid -> condition_levelのようなことを api/trendのためにしたかった.

var isuMeta struct {
	ID        int    `db:"id"`
	Character string `db:"character"`
}

type isuMetaCacheEntry struct {
	IsuID     int
	Character string
}

// jia_isu_uuid -> isu_meta 更新中なのか否かの管理が必要らしい, 大変...

type IsuMetaCache struct {
	mu sync.RWMutex
	m  map[string]isuMetaCacheEntry
}

var isuMetaCache = IsuMetaCache{
	m: make(map[string]isuMetaCacheEntry),
}

var activatingIsuCache = struct {
	mu sync.RWMutex
	m  map[string]struct{}
}{
	m: make(map[string]struct{}),
}

func clearActivatingIsuCache() {
	activatingIsuCache.mu.Lock()
	activatingIsuCache.m = make(map[string]struct{})
	activatingIsuCache.mu.Unlock()
}

func rebuildIsuMetaCache() error {
	isuList := []Isu{}
	if err := db.Select(&isuList, "select id, jia_isu_uuid, `character` from isu"); err != nil {
		return err
	}

	next := make(map[string]isuMetaCacheEntry, len(isuList))
	for _, isu := range isuList {
		next[isu.JIAIsuUUID] = isuMetaCacheEntry{
			IsuID:     isu.ID,
			Character: isu.Character,
		}
	}

	isuMetaCache.mu.Lock()
	isuMetaCache.m = next
	isuMetaCache.mu.Unlock()

	return nil
}

func rebuildLatestConditionCache() error {
	type LatestConditionRow struct {
		JIAIsuUUID string    `db:"jia_isu_uuid"`
		Timestamp  time.Time `db:"timestamp"`
		IsSitting  bool      `db:"is_sitting"`
		Condition  string    `db:"condition"`
		Level      string    `db:"level"`
		Message    string    `db:"message"`
	}

	rows := []LatestConditionRow{}
	err := db.Select(&rows,
		"SELECT ic.jia_isu_uuid, ic.timestamp, ic.is_sitting, ic.condition, ic.level, ic.message "+
			"from isu_condition ic "+
			"INNER JOIN ("+
			"  SELECT `jia_isu_uuid`, MAX(`timestamp`) AS `timestamp` "+
			"  FROM `isu_condition` "+
			"  GROUP BY `jia_isu_uuid`"+
			") latest "+
			"ON ic.`jia_isu_uuid` = latest.`jia_isu_uuid` "+
			"AND ic.`timestamp` = latest.`timestamp`",
	)
	if err != nil {
		return err
	}

	next := make(map[string]latestConditionCacheEntry, len(rows))
	for _, row := range rows {
		next[row.JIAIsuUUID] = latestConditionCacheEntry{
			Timestamp: row.Timestamp,
			IsSitting: row.IsSitting,
			Condition: row.Condition,
			Level:     row.Level,
			Message:   row.Message,
		}
	}

	latestConditionCache.ReplaceAll(next)

	return nil
}

func main() {
	e := echo.New()
	e.Debug = true
	e.Logger.SetLevel(log.DEBUG)

	// こちらをcomment outして, journaldのふかを下げる
	// e.Use(middleware.Logger())
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

	e.GET("/", getIndex)
	e.GET("/isu/:jia_isu_uuid", getIndex)
	e.GET("/isu/:jia_isu_uuid/condition", getIndex)
	e.GET("/isu/:jia_isu_uuid/graph", getIndex)
	e.GET("/register", getIndex)
	e.Static("/assets", frontendContentsPath+"/assets")

	mySQLConnectionData = NewMySQLConnectionEnv()

	var err error
	db, err = mySQLConnectionData.ConnectDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	db.SetMaxOpenConns(10)
	defer db.Close()

	postIsuConditionTargetBaseURL = os.Getenv("POST_ISUCONDITION_TARGET_BASE_URL")
	if postIsuConditionTargetBaseURL == "" {
		e.Logger.Fatalf("missing: POST_ISUCONDITION_TARGET_BASE_URL")
		return
	}

	// pprof用のサーバーを追加
	go func() {
		e.Logger.Fatal(http.ListenAndServe("0.0.0.0:6060", nil))
	}()

	serverPort := fmt.Sprintf(":%v", getEnv("SERVER_APP_PORT", "3000"))
	// 前回のbenchmarkプロセスのCLOSE-WAITが残り続けてしまっていたらしい,詳細未調査
	e.Server.ReadHeaderTimeout = 2 * time.Second
	e.Server.WriteTimeout = 3 * time.Second
	e.Server.IdleTimeout = 5 * time.Second
	e.Logger.Fatal(e.Start(serverPort))
}

func getSession(r *http.Request) (*sessions.Session, error) {
	session, err := sessionStore.Get(r, sessionName) // sessionName(const) = isuconditionGo, Cookieのことですか？
	// なんか echoの機能があるみたく, NewCookieStore()みたいなのをinitで呼んでる.
	// 多分このsessionStoreからのkeyでの取り出し自体が, gorilla/sesionによる KEYでの検証を隠蔽してるので、このデータが多分信頼できる
	if err != nil {
		return nil, err
	}
	return session, nil
}

// TODO: jwtの中にuser_idを持たせたほうが良さそう. 毎回DB引いてやがる
// TO CHECK: jwtのverify結果のcachingをしてみる?? (メモリーにかなり余裕あるし)
func getUserIDFromSession(c echo.Context) (string, int, error) {
	session, err := getSession(c.Request())
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("failed to get session: %v", err)
	}
	_jiaUserID, ok := session.Values["jia_user_id"]
	if !ok {
		return "", http.StatusUnauthorized, fmt.Errorf("no session")
	}

	jiaUserID := _jiaUserID.(string)
	//var count int

	// この処理は何をされてる方なの? userが退会したことを懸念してるの？
	// 退会APIはあるの？ → なさそうだったので、この処理全部消してみる
	//err = db.Get(&count, "SELECT COUNT(*) FROM `user` WHERE `jia_user_id` = ?",
	//	jiaUserID)
	//if err != nil {
	//	return "", http.StatusInternalServerError, fmt.Errorf("db error: %v", err)
	//}

	//if count == 0 {
	//	return "", http.StatusUnauthorized, fmt.Errorf("not found: user")
	//}

	return jiaUserID, 0, nil
}

func getJIAServiceURL(tx *sqlx.Tx) string {
	var config Config
	err := tx.Get(&config, "SELECT * FROM `isu_association_config` WHERE `name` = ?", "jia_service_url")
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Print(err)
		}
		return defaultJIAServiceURL
	}
	return config.URL
}

// ファイル書き出しのhelper
func saveIsuIconToFile(jiaIsuUUID string, image []byte) error {
	if err := os.MkdirAll(iconFileDir, 0755); err != nil {
		return err
	}

	iconPath := filepath.Join(iconFileDir, jiaIsuUUID+".jpg")
	return ioutil.WriteFile(iconPath, image, 0644)
}

// init後にimageをfileに書き出す
func exportIsuIconsToFile() error {
	rows, err := db.Queryx("SELECT jia_isu_uuid, image FROM isu")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var isu struct {
			JIAIsuUUID string `db:"jia_isu_uuid"`
			Image      []byte `db:"image"`
		}
		if err := rows.StructScan(&isu); err != nil {
			return err
		}
		if err := saveIsuIconToFile(isu.JIAIsuUUID, isu.Image); err != nil {
			return err
		}
	}

	return rows.Err()
}

// 環境を初期化するため,書き出されたiconを消す
func resetIsuIconFiles() error {
	if err := os.RemoveAll(iconFileDir); err != nil {
		return err
	}
	return os.MkdirAll(iconFileDir, 0755)
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
	jiaServiceURL = request.JIAServiceURL

	clearActivatingIsuCache()

	// in-memoryのcacheデータの再構築
	if err := rebuildIsuMetaCache(); err != nil {
		c.Logger().Errorf("faile to rebuild isu meta cache: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if err := rebuildLatestConditionCache(); err != nil {
		c.Logger().Errorf("failed to rebuild latest condition cache: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// このinit処理は20秒の猶予があるらしい,
	// ファイル書き出し処理をしても余裕で間に合うだろう

	if err := resetIsuIconFiles(); err != nil {
		c.Logger().Errorf("failed to reset isu icons: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if err := exportIsuIconsToFile(); err != nil {
		c.Logger().Errorf("faile to export isu icons: %v", err)
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

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	isuList := []Isu{}
	// ここで*とすると,longblobのimageも毎回引っ張るので重そうだから、指定してみる
	err = tx.Select(
		&isuList,
		"SELECT `id`, `jia_isu_uuid`, `name`, `character` FROM `isu` WHERE `jia_user_id` = ? ORDER BY `id` DESC",
		jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	responseList := []GetIsuListResponse{}
	for _, isu := range isuList {
		var formattedCondition *GetIsuConditionResponse
		lastCondition, ok := latestConditionCache.Get(isu.JIAIsuUUID)
		if ok {
			formattedCondition = &GetIsuConditionResponse{
				JIAIsuUUID:     isu.JIAIsuUUID,
				IsuName:        isu.Name,
				Timestamp:      lastCondition.Timestamp.Unix(),
				IsSitting:      lastCondition.IsSitting,
				Condition:      lastCondition.Condition,
				ConditionLevel: lastCondition.Level,
				Message:        lastCondition.Message,
			}
		}

		res := GetIsuListResponse{
			ID:                 isu.ID,
			JIAIsuUUID:         isu.JIAIsuUUID,
			Name:               isu.Name,
			Character:          isu.Character,
			LatestIsuCondition: formattedCondition}
		responseList = append(responseList, res)
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
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

	isuMetaCache.mu.RLock()
	_, exists := isuMetaCache.m[jiaIsuUUID]
	isuMetaCache.mu.RUnlock()
	if exists {
		return c.String(http.StatusConflict, "duplicated: isu")
	}

	activatingIsuCache.mu.Lock()
	activatingIsuCache.m[jiaIsuUUID] = struct{}{}
	activatingIsuCache.mu.Unlock()
	defer func() {
		activatingIsuCache.mu.Lock()
		delete(activatingIsuCache.m, jiaIsuUUID)
		activatingIsuCache.mu.Unlock()
	}()

	targetURL := jiaServiceURL + "/api/activate"
	body := JIAServiceRequest{postIsuConditionTargetBaseURL, jiaIsuUUID}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// TODO: 今気づいたけど、これなんか怪しいな, NewRequestって,
	// syntax知らんけども, ちゃんとソケット再活用してるんか？
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

	if err := saveIsuIconToFile(jiaIsuUUID, image); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	var result sql.Result
	for retry := 0; retry < 3; retry++ {
		result, err = db.Exec("INSERT INTO `isu`"+
			"	(`jia_isu_uuid`, `name`, `image`, `character`, `jia_user_id`) VALUES (?, ?, ?, ?, ?)",
			jiaIsuUUID, isuName, image, isuFromJIA.Character, jiaUserID)
		if err == nil {
			break
		}

		mysqlErr, ok := err.(*mysql.MySQLError)
		if ok && mysqlErr.Number == uint16(mysqlErrNumDuplicateEntry) {
			return c.String(http.StatusConflict, "duplicated: isu")
		}
		if ok && mysqlErr.Number == 1213 {
			time.Sleep(time.Duration(retry+1) * 10 * time.Millisecond)
			continue
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	isuID, err := result.LastInsertId()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	isu := Isu{
		ID:         int(isuID),
		JIAIsuUUID: jiaIsuUUID,
		Name:       isuName,
		Character:  isuFromJIA.Character,
		JIAUserID:  jiaUserID,
	}

	// ここでキャッシュ更新をするお
	isuMetaCache.mu.Lock()
	isuMetaCache.m[jiaIsuUUID] = isuMetaCacheEntry{
		IsuID:     isu.ID,
		Character: isu.Character,
	}
	isuMetaCache.mu.Unlock()

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

	var res Isu
	err = db.Get(&res, "SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
		jiaUserID, jiaIsuUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSON(http.StatusOK, res)
}

// GET /api/isu/:jia_isu_uuid/icon
// ISUのアイコンを取得
// 毎回DBを叩くな
// 変更とかは一切ないので、楽にキャッシュができそう
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

	//var image []byte
	//err = db.Get(&image, "SELECT `image` FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
	//	jiaUserID, jiaIsuUUID)

	// 画像取得ではなく存在判定のみ
	var exists bool
	err = db.Get(&exists,
		"SELECT EXISTS(SELECT 1 FROM isu where jia_user_id = ? and jia_isu_uuid = ?)",
		jiaUserID, jiaIsuUUID,
	)

	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if !exists {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	// iconPath := filepath(iconFileDir, jiaIsuUUID+".jpg")
	// return c.File(iconPath) あんまり早くならんかも
	//	return c.Blob(http.StatusOK, "", image)

	// nginxにredirectする
	iconPath := "/internal-icons/" + jiaIsuUUID + ".jpg"
	c.Response().Header().Set("X-Accel-Redirect", iconPath)
	c.Response().Header().Set("Content-Type", "image/jpeg")
	// Cache feature
	c.Response().Header().Set("Cache-Control", "private, max-age=31536000, imutable")

	return c.NoContent(http.StatusOK)
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

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	var count int
	err = tx.Get(&count, "SELECT COUNT(*) FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?", // user_idのidxはないが、jia_isu_uuidがUNIQUEなため非常に高速
		jiaUserID, jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	if count == 0 {
		return c.String(http.StatusNotFound, "not found: isu")
	}

	res, err := generateIsuGraphResponse(tx, jiaIsuUUID, date)
	if err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.JSON(http.StatusOK, res)
}

// グラフのデータ点を一日分生成
// １日分を１時間毎に生成してそうだった。GUIを見ると
// この結果は容易にキャッシュできそうでは？と思ったが、ヒット率は低いかな？
func generateIsuGraphResponse(tx *sqlx.Tx, jiaIsuUUID string, graphDate time.Time) ([]GraphResponse, error) {
	dataPoints := []GraphDataPointWithInfo{}
	conditionsInThisHour := []IsuCondition{}
	timestampsInThisHour := []int64{}
	var startTimeInThisHour time.Time
	var condition IsuCondition

	// 日時指定をかけ、該当date以外の無駄な処理を省く
	startTime := graphDate
	endTime := graphDate.Add(time.Hour * 24)
	rows, err := tx.Queryx("SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ? "+
		"AND timestamp >= ? "+
		"AND timestamp < ? "+
		"ORDER BY `timestamp` ASC", jiaIsuUUID, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		err = rows.StructScan(&condition)
		if err != nil {
			return nil, err
		}

		truncatedConditionTime := condition.Timestamp.Truncate(time.Hour) // truncate min, seconds, only hour! date + hour:00:00
		if truncatedConditionTime != startTimeInThisHour {
			if len(conditionsInThisHour) > 0 {
				data, err := calculateGraphDataPoint(conditionsInThisHour)
				if err != nil {
					return nil, err
				}

				dataPoints = append(dataPoints,
					GraphDataPointWithInfo{
						JIAIsuUUID:          jiaIsuUUID,
						StartAt:             startTimeInThisHour,
						Data:                data,
						ConditionTimestamps: timestampsInThisHour})
			}

			startTimeInThisHour = truncatedConditionTime
			conditionsInThisHour = []IsuCondition{}
			timestampsInThisHour = []int64{}
		}
		conditionsInThisHour = append(conditionsInThisHour, condition)
		timestampsInThisHour = append(timestampsInThisHour, condition.Timestamp.Unix())
	}

	if len(conditionsInThisHour) > 0 {
		data, err := calculateGraphDataPoint(conditionsInThisHour)
		if err != nil {
			return nil, err
		}

		dataPoints = append(dataPoints,
			GraphDataPointWithInfo{
				JIAIsuUUID:          jiaIsuUUID,
				StartAt:             startTimeInThisHour,
				Data:                data,
				ConditionTimestamps: timestampsInThisHour})
	}

	// endTime := graphDate.Add(time.Hour * 24)
	startIndex := len(dataPoints)
	endNextIndex := len(dataPoints)
	for i, graph := range dataPoints {
		if startIndex == len(dataPoints) && !graph.StartAt.Before(graphDate) {
			startIndex = i
		}
		if endNextIndex == len(dataPoints) && graph.StartAt.After(endTime) {
			endNextIndex = i
		}
	}

	filteredDataPoints := []GraphDataPointWithInfo{}
	if startIndex < endNextIndex {
		filteredDataPoints = dataPoints[startIndex:endNextIndex]
	}

	responseList := []GraphResponse{}
	index := 0
	thisTime := graphDate

	for thisTime.Before(graphDate.Add(time.Hour * 24)) {
		var data *GraphDataPoint
		timestamps := []int64{}

		if index < len(filteredDataPoints) {
			dataWithInfo := filteredDataPoints[index]

			if dataWithInfo.StartAt.Equal(thisTime) {
				data = &dataWithInfo.Data
				timestamps = dataWithInfo.ConditionTimestamps
				index++
			}
		}

		resp := GraphResponse{
			StartAt:             thisTime.Unix(),
			EndAt:               thisTime.Add(time.Hour).Unix(),
			Data:                data,
			ConditionTimestamps: timestamps,
		}
		responseList = append(responseList, resp)

		thisTime = thisTime.Add(time.Hour)
	}

	return responseList, nil
}

// 複数のISUのコンディションからグラフの一つのデータ点を計算
func calculateGraphDataPoint(isuConditions []IsuCondition) (GraphDataPoint, error) {
	conditionsCount := map[string]int{"is_broken": 0, "is_dirty": 0, "is_overweight": 0}
	rawScore := 0
	for _, condition := range isuConditions {
		badConditionsCount := 0

		if !isValidConditionFormat(condition.Condition) {
			return GraphDataPoint{}, fmt.Errorf("invalid condition format")
		}

		for _, condStr := range strings.Split(condition.Condition, ",") {
			keyValue := strings.Split(condStr, "=")

			conditionName := keyValue[0]
			if keyValue[1] == "true" {
				conditionsCount[conditionName] += 1
				badConditionsCount++
			}
		}

		if badConditionsCount >= 3 {
			rawScore += scoreConditionLevelCritical
		} else if badConditionsCount >= 1 {
			rawScore += scoreConditionLevelWarning
		} else {
			rawScore += scoreConditionLevelInfo
		}
	}

	sittingCount := 0
	for _, condition := range isuConditions {
		if condition.IsSitting {
			sittingCount++
		}
	}

	isuConditionsLength := len(isuConditions)

	score := rawScore * 100 / 3 / isuConditionsLength

	sittingPercentage := sittingCount * 100 / isuConditionsLength
	isBrokenPercentage := conditionsCount["is_broken"] * 100 / isuConditionsLength
	isOverweightPercentage := conditionsCount["is_overweight"] * 100 / isuConditionsLength
	isDirtyPercentage := conditionsCount["is_dirty"] * 100 / isuConditionsLength

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

// GET /api/condition/:jia_isu_uuid
// ISUのコンディションを取得
// これは豪華な検索クエリーだね
// Params: jia_isu_uuid, start_time, end_time,
// from Cookie: user_id, -> isu_name
// Constant: conditionLimit = 20 が limitしている.
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

	var isuName string
	err = db.Get(&isuName,
		"SELECT name FROM `isu` WHERE `jia_isu_uuid` = ? AND `jia_user_id` = ?",
		jiaIsuUUID, jiaUserID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.String(http.StatusNotFound, "not found: isu")
		}

		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// TO CHECK: ここのクエリーにname必要なのか？
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

	conditionLevelArray := []string{}
	for level, _ := range conditionLevel {
		conditionLevelArray = append(conditionLevelArray, level)
	}

	conditions := []IsuCondition{}
	var query string
	var params []interface{}
	var err error

	if startTime.IsZero() {
		// いったん * -> 必要なデータをやってみる
		query, params, err = sqlx.In(
			"SELECT `jia_isu_uuid`,`timestamp`,`is_sitting`,`condition`,`level`,"+
				"`message` FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"       AND `level` IN (?)"+ // 配列を展開するためにsqlx.Inを使っているらしい
				"	ORDER BY `timestamp` DESC LIMIT ?",
			jiaIsuUUID, endTime, conditionLevelArray, limit,
		)
	} else {
		query, params, err = sqlx.In(
			"SELECT `jia_isu_uuid`,`timestamp`,`is_sitting`,`condition`,`level`,"+
				"`message`  FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"	AND ? <= `timestamp`"+
				"       AND `level` IN (?)"+ // 配列を展開するためにsqlx.Inを使っているらしい
				"	ORDER BY `timestamp` DESC LIMIT ?",
			jiaIsuUUID, endTime, startTime, conditionLevelArray, limit,
		)
	}

	// log追加, debug
	// fmt.Printf("query=%s params=%v\n", query, params)

	if err != nil {
		return nil, fmt.Errorf("db error: %v", err)
	}

	err = db.Select(&conditions, db.Rebind(query), params...)
	if err != nil {
		return nil, err
	}

	conditionsResponse := []*GetIsuConditionResponse{}
	for _, c := range conditions {
		// 		cLevel, err := calculateConditionLevel(c.Condition)
		//		if err != nil {
		//			continue}

		//----------- DEBUG--------
		//		if cLevel != c.Level {
		//		fmt.Printf("data inconsistency: condition=%s level=%s cLevel=%s\n",
		//   c.Condition, c.Level, cLevel)
		//fmt.Printf("fucking data inconsistency, %s, %s\n", c.Condition, c.Level)
		//		}

		data := GetIsuConditionResponse{
			JIAIsuUUID: c.JIAIsuUUID,
			IsuName:    isuName,
			Timestamp:  c.Timestamp.Unix(),
			IsSitting:  c.IsSitting,
			Condition:  c.Condition,
			//ConditionLevel: cLevel,
			ConditionLevel: c.Level,
			Message:        c.Message,
		}

		//if len(conditionsResponse) > limit {
		conditionsResponse = append(conditionsResponse, &data) // 最初からその件数になっている.
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

// GET /api/trend
// ISUの性格毎の最新のコンディション情報
func getTrend(c echo.Context) error {
	// 一度に全部のisuを取得してしまう, N+1の簡単な解消
	//	allIsuList := []Isu{}
	//	err := db.Select(&allIsuList, "SELECT id, `character`, jia_isu_uuid from isu")
	//	if err != nil {
	//		c.Logger().Errorf("db error: %v", err)
	//		return c.NoContent(http.StatusInternalServerError)
	//	}

	// せっかく作ったキャッシュから取得する
	isuMetaCache.mu.RLock()
	allIsuList := make([]Isu, 0, len(isuMetaCache.m))
	for jiaIsuUUID, meta := range isuMetaCache.m {
		allIsuList = append(allIsuList, Isu{
			ID:         meta.IsuID,
			JIAIsuUUID: jiaIsuUUID,
			Character:  meta.Character,
		})
	}
	isuMetaCache.mu.RUnlock()

	characterToIsuList := make(map[string][]Isu)
	for _, isu := range allIsuList {
		characterToIsuList[isu.Character] = append(characterToIsuList[isu.Character], isu)
	}

	res := []TrendResponse{}

	latestSnapshot := latestConditionCache.Snapshot()

	for character, isuList := range characterToIsuList {
		//isuList := []Isu{}
		//err = db.Select(&isuList,
		// "SELECT * FROM `isu` WHERE `character` = ?",
		//	"SELECT id, jia_isu_uuid FROM `isu` WHERE `character` = ?",
		//	character.Character,
		//	)
		//	if err != nil {
		//		c.Logger().Errorf("db error: %v", err)
		//		return c.NoContent(http.StatusInternalServerError)
		//	}

		characterInfoIsuConditions := []*TrendCondition{}
		characterWarningIsuConditions := []*TrendCondition{}
		characterCriticalIsuConditions := []*TrendCondition{}
		for _, isu := range isuList {
			//conditions := []IsuCondition{}
			// LIMIT 1
			// * -> timestamp, condition
			//err = db.Select(&conditions,
			//	"SELECT `timestamp`, `condition` FROM `isu_condition` WHERE `jia_isu_uuid` = ? ORDER BY timestamp DESC LIMIT 1",
			//	isu.JIAIsuUUID,
			//)
			//if err != nil {
			//	c.Logger().Errorf("db error: %v", err)
			//	return c.NoContent(http.StatusInternalServerError)
			//}

			//if len(conditions) > 0 {
			//	isuLastCondition := conditions[0]
			//	conditionLevel, err := calculateConditionLevel(isuLastCondition.Condition)
			//	if err != nil {
			//		c.Logger().Error(err)
			//		return c.NoContent(http.StatusInternalServerError)
			//	}
			//	trendCondition := TrendCondition{
			//		ID:        isu.ID,
			//		Timestamp: isuLastCondition.Timestamp.Unix(),
			//	}

			// isuに対応する最新のconditionをin-memory cacheより取得する
			// 取っておいたsnapshotから読む
			latestCondition, ok := latestSnapshot[isu.JIAIsuUUID]
			if !ok {
				continue // これはcontinueで良いんだっけ？別にいい、前の実装もこうだった, conditionがないこともある,
			}
			conditionLevel := latestCondition.Level
			trendCondition := TrendCondition{
				ID:        isu.ID,
				Timestamp: latestCondition.Timestamp.Unix(),
			}

			switch conditionLevel {
			case "info":
				characterInfoIsuConditions = append(characterInfoIsuConditions, &trendCondition)
			case "warning":
				characterWarningIsuConditions = append(characterWarningIsuConditions, &trendCondition)
			case "critical":
				characterCriticalIsuConditions = append(characterCriticalIsuConditions, &trendCondition)
				//	}
			}

		}

		sort.Slice(characterInfoIsuConditions, func(i, j int) bool {
			return characterInfoIsuConditions[i].Timestamp > characterInfoIsuConditions[j].Timestamp
		})
		sort.Slice(characterWarningIsuConditions, func(i, j int) bool {
			return characterWarningIsuConditions[i].Timestamp > characterWarningIsuConditions[j].Timestamp
		})
		sort.Slice(characterCriticalIsuConditions, func(i, j int) bool {
			return characterCriticalIsuConditions[i].Timestamp > characterCriticalIsuConditions[j].Timestamp
		})
		res = append(res,
			TrendResponse{
				Character: character,
				Info:      characterInfoIsuConditions,
				Warning:   characterWarningIsuConditions,
				Critical:  characterCriticalIsuConditions,
			})
	}

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

	req := []PostIsuConditionRequest{} // 配列でくるのね
	err := c.Bind(&req)
	if err != nil {
		return c.String(http.StatusBadRequest, "bad request body")
	} else if len(req) == 0 {
		return c.String(http.StatusBadRequest, "bad request body")
	}

	isuMetaCache.mu.RLock()
	isuMeta, ok := isuMetaCache.m[jiaIsuUUID]
	isuMetaCache.mu.RUnlock()
	if !ok {
		activatingIsuCache.mu.RLock()
		_, activating := activatingIsuCache.m[jiaIsuUUID]
		activatingIsuCache.mu.RUnlock()
		if activating {
			return c.NoContent(http.StatusAccepted)
		}
		return c.String(http.StatusNotFound, "not found: isu")
	}

	var rows []IsuCondition
	var latestCondition IsuCondition
	hasLatestCondition := false
	for _, cond := range req {
		timestamp := time.Unix(cond.Timestamp, 0)

		if !isValidConditionFormat(cond.Condition) {
			return c.String(http.StatusBadRequest, "bad request body")
		}

		// 前計算
		cLevel, err := calculateConditionLevel(cond.Condition)
		if err != nil {
			return c.String(http.StatusBadRequest, "bad request body")
		}
		condition := IsuCondition{
			JIAIsuUUID: jiaIsuUUID,
			Timestamp:  timestamp,
			IsSitting:  cond.IsSitting,
			Condition:  cond.Condition,
			Level:      cLevel, // 冗長的に持たせる
			Message:    cond.Message,
		}
		rows = append(rows, condition)
		if !hasLatestCondition || latestCondition.Timestamp.Before(condition.Timestamp) {
			latestCondition = condition
			hasLatestCondition = true
		}
	}

	//	tx, err := db.Beginx() //DBアクセスも1つのbulk insertだけになったし, txを消す
	//	if err != nil {
	//		c.Logger().Errorf("db error: %v", err)
	//		return c.NoContent(http.StatusInternalServerError)
	//	}
	//	defer tx.Rollback()

	// bulk insertする
	_, err = db.NamedExec(
		"INSERT INTO `isu_condition`"+
			"	(`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`,`level`, `message`)"+
			"	VALUES (:jia_isu_uuid, :timestamp, :is_sitting, :condition, :level, :message)",
		rows)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	// do we still need this commit for this bulk update??
	//err = tx.Commit()

	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	if hasLatestCondition {
		latestConditionCache.SetIfNewer(
			jiaIsuUUID,
			latestConditionCacheEntry{
				IsuID:     isuMeta.IsuID,
				Character: isuMeta.Character,
				Timestamp: latestCondition.Timestamp,
				IsSitting: latestCondition.IsSitting,
				Condition: latestCondition.Condition,
				Level:     latestCondition.Level,
				Message:   latestCondition.Message,
			},
		)
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

//GET /api/trend
//ISUの性格毎の最新のコンディション情報
//func getTrend(c echo.Context) error {
//	// 1. 全ISUを一度に取得
//	isuList := []Isu{}
//	err := db.Select(&isuList, "SELECT * FROM `isu`")
//	if err != nil {
//		c.Logger().Errorf("db error: %v", err)
//		return c.NoContent(http.StatusInternalServerError)
//	}
//
//	// 2. 各ISUの最新conditionを一度に取得
//	conditions := []IsuCondition{}
//	err = db.Select(&conditions, `
//		SELECT ic.*
//		FROM `+"`isu_condition`"+` ic
//		JOIN (
//			SELECT `+"`jia_isu_uuid`"+`, MAX(`+"`timestamp`"+`) AS max_timestamp
//			FROM `+"`isu_condition`"+`
//			GROUP BY `+"`jia_isu_uuid`"+`
//		) latest
//		  ON ic.`+"`jia_isu_uuid`"+` = latest.`+"`jia_isu_uuid`"+`
//		 AND ic.`+"`timestamp`"+` = latest.max_timestamp
//	`)
//	if err != nil {
//		c.Logger().Errorf("db error: %v", err)
//		return c.NoContent(http.StatusInternalServerError)
//	}
//
//	// 3. jia_isu_uuid -> 最新condition のMapを作る
//	conditionMap := make(map[string]IsuCondition, len(conditions))
//	for _, condition := range conditions {
//		conditionMap[condition.JIAIsuUUID] = condition
//	}
//
//	// 4. character -> TrendResponse のMapを作る
//	trendMap := make(map[string]*TrendResponse)
//
//	// 5. 全ISUをGo側でcharacterごとに分類
//	for _, isu := range isuList {
//		// characterごとのResponseを初期化
//		if _, ok := trendMap[isu.Character] = &TrendResponse{
//				Character: isu.Character,
//				Info:      []*TrendCondition{},
//				Warning:   []*TrendCondition{},
//				Critical:  []*TrendCondition{},
//			}
//		}
//
//		// 最新conditionが存在しなければスキップ
//		condition, ok := conditionMap[isu.JIAIsuUUID]
//		if !ok {
//			continue
//		}
//
//		conditionLevel, err := calculateConditionLevel(condition.Condition)
//		if err != nil {
//			c.Logger().Error(err)
//			return c.NoContent(http.StatusInternalServerError)
//		}
//
//		trendCondition := &TrendCondition{
//			ID:        isu.ID,
//			Timestamp: condition.Timestamp.Unix(),
//		}
//
//		switch conditionLevel {
//		case "info":
//			trendMap[isu.Character].Info =
//				append(trendMap[isu.Character].Info, trendCondition)
//
//		case "warning":
//			trendMap[isu.Character].Warning =
//				append(trendMap[isu.Character].Warning, trendCondition)
//
//		case "critical":
//			trendMap[isu.Character].Critical =
//				append(trendMap[isu.Character].Critical, trendCondition)
//		}
//	}
//
//	// 6. 元のコードと同じようにcharacterごとにTimestamp降順でsort
//	res := make([]TrendResponse, 0, len(trendMap))
//
//	for _, trend := range trendMap {
//		sort.Slice(trend.Info, func(i, j int) bool {
//			return trend.Info[i].Timestamp > trend.Info[j].Timestamp
//		})
//
//		sort.Slice(trend.Warning, func(i, j int) bool {
//			return trend.Warning[i].Timestamp > trend.Warning[j].Timestamp
//		})
//
//		sort.Slice(trend.Critical, func(i, j int) bool {
//			return trend.Critical[i].Timestamp > trend.Critical[j].Timestamp
//		})
//
//		res = append(res, *trend)
//	}
//
//	return c.JSON(http.StatusOK, res)
//}
