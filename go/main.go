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

	// jia_isu_uuid -> isuのmeta
	// 存在確認, 認可判定に活用
	isuMetaMu     sync.RWMutex
	isuMetaByUUID map[string]IsuMeta

	sessionUserCache sync.Map // session cookie string -> jia_user_id
	// skipping the cryptographic calculations
)

type IsuMeta struct {
	ID         int
	JIAIsuUUID string `db:"jia_isu_uuid"`
	Name       string
	Character  string
	JIAUserID  string `db:"jia_user_id"`
}

func loadIsuMetaCache() error {
	var rows []IsuMeta
	err := db.Select(&rows, `
		SELECT id, jia_isu_uuid, name, `+"`character`"+`, jia_user_id
		FROM isu
	`)
	if err != nil {
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

/// when isu added to DB
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
		if err := rows.StructScan(&isu); err != nil {
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
	db.SetMaxOpenConns(20) // TODO:大きくした方がスコアが高くなりやすそう, あとでこれの意義と最適なものを探る, 負荷状況、ボトルネックの場所によって最適な値が変わる
	defer db.Close()

	postIsuConditionTargetBaseURL = os.Getenv("POST_ISUCONDITION_TARGET_BASE_URL")
	if postIsuConditionTargetBaseURL == "" {
		e.Logger.Fatalf("missing: POST_ISUCONDITION_TARGET_BASE_URL")
		return
	}

	// global変数のjiaURLに代入する
	var config Config
	err = db.Get(&config, "SELECT * FROM `isu_association_config` WHERE `name` = ?", "jia_service_url")
	jiaURL = config.URL
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

	if err := loadIsuMetaCache(); err != nil {
		c.Logger().Errorf("failed to load isu meta cache: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	invalidateTrendCache()
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

	type IsuListRow struct {
		ID                  int            `db:"id"`
		JIAIsuUUID          string         `db:"jia_isu_uuid"`
		Name                string         `db:"name"`
		Character           string         `db:"character"`
		LatestTimestamp     sql.NullTime   `db:"latest_timestamp"`
		LatestIsSitting     sql.NullBool   `db:"latest_is_sitting"`
		LatestConditionBits sql.NullInt64  `db:"latest_condition_bits"`
		LatestLevelInt      sql.NullInt64  `db:"latest_level_int"`
		LatestMessage       sql.NullString `db:"latest_message"`
	}
	isuList := []IsuListRow{}
	err = db.Select(
		&isuList,
		"SELECT i.id, i.jia_isu_uuid, i.name, i.`character`, "+
			" l.timestamp as latest_timestamp,"+
			" l.is_sitting as latest_is_sitting,"+
			" l.`condition_bits` as latest_condition_bits,"+
			" l.level_int as latest_level_int,"+
			" l.message as latest_message"+
			" FROM `isu` i "+
			" LEFT JOIN latest_isu_condition l"+
			" ON l.jia_isu_uuid = i.jia_isu_uuid"+
			" WHERE i.jia_user_id = ?"+
			" ORDER BY i.id DESC",
		jiaUserID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	responseList := []GetIsuListResponse{}

	for _, isu := range isuList {
		var formattedCondition *GetIsuConditionResponse
		if isu.LatestTimestamp.Valid {
			formattedCondition = &GetIsuConditionResponse{
				JIAIsuUUID:     isu.JIAIsuUUID,
				IsuName:        isu.Name,
				Timestamp:      isu.LatestTimestamp.Time.Unix(),
				IsSitting:      isu.LatestIsSitting.Bool,
				Condition:      conditionStringFromBits(int(isu.LatestConditionBits.Int64)),
				ConditionLevel: levelStringFromInt(int(isu.LatestLevelInt.Int64)),
				Message:        isu.LatestMessage.String,
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

	// 失敗したら、そのファイル使われなくなるだろうし、この位置で書き込んじゃっていいかなぁ
	if saveIsuIconToFile(jiaIsuUUID, jiaUserID, image); err != nil {
		c.Logger().Error(err)
		return c.NoContent(http.StatusInternalServerError)
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
	err = tx.Get(
		&isu,
		"SELECT * FROM `isu` WHERE `jia_user_id` = ? AND `jia_isu_uuid` = ?",
		jiaUserID, jiaIsuUUID)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
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

	var res Isu
	// 一件改善できなそうに見えるこのクエリーがやたら重たい, 画像かな
	// schema側でINVISIBLEを付与してみた -> 時間は半分くらいに, あとは整合性エラーが出るのかと思ったが出なかった
	// ここで要求されてるレスポンスはなんなんだ？どこかに定義されているのか？？
	// このレスポンスの省略は, 合法なのか？
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

	c.Response().Header().Set("X-Accel-Redirect", iconPath)                           // return from nginx
	c.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable") // add cache, since the icon image is not updated.
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

func generateIsuGraphResponse(tx *sqlx.Tx, jiaIsuUUID string, graphDate time.Time) ([]GraphResponse, error) {
	dataPoints := []GraphDataPointWithInfo{}
	conditionsInThisHour := []graphConditionRow{}
	timestampsInThisHour := []int64{}
	var startTimeInThisHour time.Time

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
			conditionsInThisHour = []graphConditionRow{}
			timestampsInThisHour = []int64{}
		}

		conditionsInThisHour = append(conditionsInThisHour, condition)
		timestampsInThisHour = append(timestampsInThisHour, condition.TimestampUnix)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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

	responseList := []GraphResponse{}
	index := 0
	thisTime := graphDate

	for thisTime.Before(graphDate.Add(time.Hour * 24)) {
		var data *GraphDataPoint
		timestamps := []int64{}

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

	conditions := []IsuCondition{}
	var err error

	conditionLevels := []int{}
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
			"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
				"	AND `timestamp` < ?"+
				"       AND `level_int` in (?)"+
				"	ORDER BY `timestamp` DESC"+
				"       LIMIT ?",
			jiaIsuUUID, endTime, conditionLevels, limit,
		)
	} else {
		query, params, err = sqlx.In(
			"SELECT * FROM `isu_condition` WHERE `jia_isu_uuid` = ?"+
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

	err = db.Select(&conditions, db.Rebind(query), params...)
	if err != nil {
		return nil, err
	}

	conditionsResponse := []*GetIsuConditionResponse{}
	for _, c := range conditions {
		data := GetIsuConditionResponse{
			JIAIsuUUID:     c.JIAIsuUUID,
			IsuName:        isuName,
			Timestamp:      c.Timestamp.Unix(),
			IsSitting:      c.IsSitting,
			Condition:      conditionStringFromBits(c.ConditionBits),
			ConditionLevel: levelStringFromInt(c.LevelInt),
			Message:        c.Message,
		}
		conditionsResponse = append(conditionsResponse, &data)
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

	res := []TrendResponse{}

	type TrendRow struct {
		ID        int           `db:"id"`
		Character string        `db:"character"`
		Timestamp sql.NullTime  `db:"timestamp"` // nullな場合もあり得る. latest_conditionがないisuについて
		LevelInt  sql.NullInt64 `db:"level_int"`
	}

	var trendRows []TrendRow
	err := db.Select(&trendRows, "SELECT i.id, i.character, l.timestamp, l.level_int FROM isu i"+
		" LEFT JOIN latest_isu_condition l"+
		" ON l.jia_isu_uuid = i.jia_isu_uuid"+
		" ORDER BY i.character",
	)

	if err != nil {
		return err
	}

	byCharacter := map[string]*TrendResponse{}
	for _, trendRow := range trendRows {
		tr, ok := byCharacter[trendRow.Character]
		if !ok {
			tr = &TrendResponse{
				Character: trendRow.Character,
				Info:      []*TrendCondition{},
				Warning:   []*TrendCondition{},
				Critical:  []*TrendCondition{},
			}
			byCharacter[trendRow.Character] = tr
		}
		if !trendRow.Timestamp.Valid || !trendRow.LevelInt.Valid {
			continue
		}
		tc := &TrendCondition{
			ID:        trendRow.ID,
			Timestamp: trendRow.Timestamp.Time.Unix(),
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

	tx, err := db.Beginx()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer tx.Rollback()

	//var count int
	//err = tx.Get(&count, "SELECT 1 FROM `isu` WHERE `jia_isu_uuid` = ?", jiaIsuUUID)
	//	if err != nil {
	//		c.Logger().Errorf("db error: %v", err)
	//		return c.NoContent(http.StatusInternalServerError)
	//	}
	if !existsIsu(jiaIsuUUID) { // in-memory cacheのみで判定
		return c.String(http.StatusNotFound, "not found: isu")
	}

	var rows []IsuCondition
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
	_, err = tx.NamedExec(
		"INSERT INTO `isu_condition`"+
			"	(`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition_bits`, `level_int`, `message`)"+
			"	VALUES (:jia_isu_uuid, :timestamp, :is_sitting, :condition_bits, :level_int, :message)",
		rows)
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	latestChanged := false
	var result sql.Result
	if latest != nil {
		// 普通のinsertだと主キーの重複で落ちる, ON DUPLICATE KEY UPDATEだと, もしなければ書き込み、あれば更新してくれるらしい
		result, err = tx.Exec(`
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
			result, err = tx.NamedExec(`
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

	err = tx.Commit()
	if err != nil {
		c.Logger().Errorf("db error: %v", err)
		return c.NoContent(http.StatusInternalServerError)
	}

	// 必ず書き込むのではなく, 更新があった時のみにする
	// if err != nil && affected > 0 { // 実はこれでスコアが伸びてしまったのだが、これは重大な誤り, errがある場合はcache更新すべきタイミングではない(少なくともアプリの論理的には)
	if err != nil && !latestChanged { // あえて全然更新しないロジックへ
		invalidateTrendCache()
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
