// 「正しいが意図的に遅い」初期実装。current_count / status / last_joined_at は派生計算、
// 性能目的の二次 index 無し、N+1、同期 webhook 送信。critical エラーは踏まないよう
// トランザクション + FOR UPDATE で序盤からきっちり守る。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// users.credit_limit の初期値 (docs/idea.md)。全 user 共通の固定値。
const defaultCreditLimit = 60000

const maxRequestBody = 1024 * 1024

type dbConn struct {
	host     string
	port     int
	user     string
	password string
	database string
}

type app struct {
	db     *sqlx.DB
	sqlDir string
	conn   dbConn
	http   *http.Client
}

func main() {
	dsn, ok := os.LookupEnv("DATABASE_URL")
	if !ok {
		dsn = "mysql://isucon:isucon@127.0.0.1:3306/nrb2026"
	}
	conn := parseDBURL(dsn)

	cfg := mysql.NewConfig()
	cfg.User = conn.user
	cfg.Passwd = conn.password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", conn.host, conn.port)
	cfg.DBName = conn.database
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Collation = "utf8mb4_unicode_ci"
	cfg.InterpolateParams = true
	cfg.Params = map[string]string{"time_zone": "'+00:00'"}

	db, err := sqlx.Connect("mysql", cfg.FormatDSN())
	if err != nil {
		log.Fatalf("connect to MySQL: %v", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)
	db.SetConnMaxIdleTime(10 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)

	sqlDir, ok := os.LookupEnv("SQL_DIR")
	if !ok {
		sqlDir = filepath.Join(sourceDir(), "sql")
	}

	// reqwest::Client::new() 相当: タイムアウト無し、host ごとの idle 接続数無制限。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = math.MaxInt32

	a := &app{
		db:     db,
		sqlDir: sqlDir,
		conn:   conn,
		http:   &http.Client{Transport: transport},
	}

	// STATIC_DIR が無ければ SPA を配信しない (dev は Vite dev server が SPA を担う)。
	// index.html が無いときは起動時に落とす。/healthz だけ通って / が壊れる状態を避けるため。
	var fallback http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if dir, ok := os.LookupEnv("STATIC_DIR"); ok {
		index := filepath.Join(dir, "index.html")
		if st, err := os.Stat(index); err != nil || !st.Mode().IsRegular() {
			log.Fatalf("STATIC_DIR index.html not found: %s (set STATIC_DIR to the Vite build dir, or unset to skip SPA fallback)", index)
		}
		fallback = spaHandler(dir, index)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("/healthz", methodNotAllowed("GET,HEAD"))

	mux.HandleFunc("POST /api/initialize", a.initialize)
	mux.HandleFunc("/api/initialize", methodNotAllowed("POST"))
	mux.HandleFunc("POST /api/users", a.createUser)
	mux.HandleFunc("/api/users", methodNotAllowed("POST"))
	mux.HandleFunc("GET /api/tags", a.listTags)
	mux.HandleFunc("/api/tags", methodNotAllowed("GET,HEAD"))

	// 認証必須 route では、method 不一致でも先に認証を通してから 405 を返す。
	mux.HandleFunc("GET /api/me", a.auth(a.getMe))
	mux.HandleFunc("/api/me", a.authedMethodNotAllowed("GET,HEAD"))
	mux.HandleFunc("GET /api/campaigns", a.auth(a.listCampaigns))
	mux.HandleFunc("POST /api/campaigns", a.auth(a.createCampaign))
	mux.HandleFunc("/api/campaigns", a.authedMethodNotAllowed("GET,HEAD,POST"))
	mux.HandleFunc("GET /api/campaigns/{id}", a.auth(a.getCampaign))
	mux.HandleFunc("/api/campaigns/{id}", a.authedMethodNotAllowed("GET,HEAD"))
	mux.HandleFunc("GET /api/campaigns/{id}/image", a.auth(a.getCampaignImage))
	mux.HandleFunc("/api/campaigns/{id}/image", a.authedMethodNotAllowed("GET,HEAD"))
	mux.HandleFunc("POST /api/campaigns/{id}/join", a.auth(a.joinCampaign))
	mux.HandleFunc("/api/campaigns/{id}/join", a.authedMethodNotAllowed("POST"))
	mux.HandleFunc("POST /api/saved_searches", a.auth(a.createSavedSearch))
	mux.HandleFunc("/api/saved_searches", a.authedMethodNotAllowed("POST"))
	mux.HandleFunc("GET /api/charges", a.auth(a.listCharges))
	mux.HandleFunc("/api/charges", a.authedMethodNotAllowed("GET,HEAD"))

	// `/api/*` の未定義 path は SPA fallback に吸わせず 404 にする (`/api/` 自体は fallback 側)。
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/" {
			fallback.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.Handle("/", fallback)

	port, ok := os.LookupEnv("PORT")
	if !ok {
		port = "8080"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		log.Fatalf("PORT must be a valid u16: %v", err)
	}
	addr := "0.0.0.0:" + port
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("bind %s: %v", addr, err)
	}
}

// sourceDir は Rust 版の CARGO_MANIFEST_DIR に相当する、ビルド時のソースディレクトリ。
func sourceDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

func parseDBURL(dsn string) dbConn {
	u, err := url.Parse(dsn)
	if err != nil {
		log.Fatalf("DATABASE_URL parse: %v", err)
	}
	c := dbConn{
		host:     u.Hostname(),
		port:     3306,
		database: strings.TrimLeft(u.Path, "/"),
	}
	if c.host == "" {
		c.host = "127.0.0.1"
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			log.Fatalf("DATABASE_URL parse: %v", err)
		}
		c.port = n
	}
	if u.User != nil {
		c.user = u.User.Username()
		c.password, _ = u.User.Password()
	}
	return c
}

// === responses ===

func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
}

func writeInternalError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		writeInternalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *app) authedMethodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		a.auth(func(w http.ResponseWriter, r *http.Request, _ string) {
			w.WriteHeader(http.StatusMethodNotAllowed)
		})(w, r)
	}
}

// === request body (errors → 400 with empty body) ===

// readJSONObject は body を JSON object として読む。フィールド名は大文字小文字を区別する。
func readJSONObject(r *http.Request) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

func fieldString(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil || s == nil {
		return "", false
	}
	return *s, true
}

func fieldInt32(m map[string]json.RawMessage, key string) (int32, bool) {
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	var n *int32
	if err := json.Unmarshal(raw, &n); err != nil || n == nil {
		return 0, false
	}
	return *n, true
}

func fieldStrings(m map[string]json.RawMessage, key string) ([]string, bool) {
	raw, ok := m[key]
	if !ok {
		return nil, false
	}
	var ps []*string
	if err := json.Unmarshal(raw, &ps); err != nil || ps == nil {
		return nil, false
	}
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p == nil {
			return nil, false
		}
		out = append(out, *p)
	}
	return out, true
}

// === middleware ===

func (a *app) auth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("X-User-Id")
		if len(values) == 0 {
			writeError(w, http.StatusUnauthorized)
			return
		}
		parsed, err := uuid.Parse(values[0])
		if err != nil {
			writeError(w, http.StatusUnauthorized)
			return
		}
		userID := parsed.String()
		var exists string
		err = a.db.GetContext(r.Context(), &exists, "SELECT id FROM users WHERE id = ?", userID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized)
			return
		}
		if err != nil {
			writeInternalError(w, err)
			return
		}
		next(w, r, userID)
	}
}

// === helpers ===

func nowUTC() time.Time {
	return time.Now().UTC()
}

func fmtDT(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

type jsonTime time.Time

func (t jsonTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + fmtDT(time.Time(t)) + `"`), nil
}

// validatePrice は POST /api/campaigns の price を検査する。
// 仕様 (docs/idea.md): 2000 ≤ price ≤ 20000。範囲外は 400。
func validatePrice(price int32) bool {
	return price >= 2000 && price <= 20000
}

// validateJPEGImageB64 は POST /api/campaigns の image (base64) を検査し、decode 後の bytes を返す。
// 仕様 (docs/idea.md): decode 失敗 / 0 byte / JPEG magic 不一致は 400、decode 後 > 204_800 byte は 413。
func validateJPEGImageB64(b64 string) ([]byte, int) {
	// encoding/base64 は CR / LF を読み飛ばすため、standard + canonical padding の判定から外れる。
	if strings.ContainsAny(b64, "\r\n") {
		return nil, http.StatusBadRequest
	}
	b, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return nil, http.StatusBadRequest
	}
	if len(b) == 0 {
		return nil, http.StatusBadRequest
	}
	if len(b) > 204_800 {
		return nil, http.StatusRequestEntityTooLarge
	}
	if len(b) < 3 || b[0] != 0xFF || b[1] != 0xD8 || b[2] != 0xFF {
		return nil, http.StatusBadRequest
	}
	return b, 0
}

// === DTOs ===

type campaignRes struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Description  string           `json:"description"`
	Price        int32            `json:"price"`
	GoalCount    int32            `json:"goal_count"`
	CurrentCount int32            `json:"current_count"`
	Tags         []string         `json:"tags"`
	Status       string           `json:"status"`
	CreatedAt    jsonTime         `json:"created_at"`
	LastJoinedAt *jsonTime        `json:"last_joined_at"`
	Participants []participantRes `json:"participants"`
}

type participantRes struct {
	UserID   string   `json:"user_id"`
	Name     string   `json:"name"`
	JoinedAt jsonTime `json:"joined_at"`
}

// === handlers ===

func (a *app) initialize(w http.ResponseWriter, r *http.Request) {
	m, ok := readJSONObject(r)
	if !ok {
		writeError(w, http.StatusBadRequest)
		return
	}
	webhookURL, ok := fieldString(m, "notification_webhook_url")
	if !ok {
		writeError(w, http.StatusBadRequest)
		return
	}

	if err := a.runMySQLFile(filepath.Join(a.sqlDir, "schema.sql")); err != nil {
		writeInternalError(w, err)
		return
	}
	// 配布版は scripts/build.sh が seed.sql を生成する (1500 件 + 画像)。
	// dev fresh checkout では seed.sql が無いので seed.base.sql に fallback (5 件)。
	seedPath := filepath.Join(a.sqlDir, "seed.sql")
	if _, err := os.Stat(seedPath); err != nil {
		seedPath = filepath.Join(a.sqlDir, "seed.base.sql")
	}
	if err := a.runMySQLFile(seedPath); err != nil {
		writeInternalError(w, err)
		return
	}

	_, err := a.db.ExecContext(r.Context(),
		"INSERT INTO app_config (name, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value = VALUES(value)",
		"notification_webhook_url", webhookURL)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (a *app) healthz(w http.ResponseWriter, r *http.Request) {
	var one int
	if err := a.db.QueryRowContext(r.Context(), "SELECT 1").Scan(&one); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// runMySQLFile は mysql CLI に path を stdin で流す。リクエストが中断されても子プロセスは止めない。
func (a *app) runMySQLFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	cmd := exec.Command("mysql",
		"-h", a.conn.host,
		"-P", strconv.Itoa(a.conn.port),
		"-u", a.conn.user,
		"--protocol=TCP",
		"--default-character-set=utf8mb4",
		a.conn.database,
	)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+a.conn.password)
	cmd.Stdin = f
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn mysql: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("mysql exit %w", err)
	}
	return nil
}

type userRes struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CreditLimit int32  `json:"credit_limit"`
}

func (a *app) createUser(w http.ResponseWriter, r *http.Request) {
	m, ok := readJSONObject(r)
	if !ok {
		writeError(w, http.StatusBadRequest)
		return
	}
	name, ok := fieldString(m, "name")
	if !ok {
		writeError(w, http.StatusBadRequest)
		return
	}
	n := utf8.RuneCountInString(name)
	if n == 0 || n > 100 {
		writeError(w, http.StatusBadRequest)
		return
	}
	id := uuid.NewString()
	now := nowUTC()
	creditLimit := int32(defaultCreditLimit)
	_, err := a.db.ExecContext(r.Context(),
		"INSERT INTO users (id, name, credit_limit, created_at) VALUES (?, ?, ?, ?)",
		id, name, creditLimit, now)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, userRes{ID: id, Name: name, CreditLimit: creditLimit})
}

type meRes struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CreditLimit int32  `json:"credit_limit"`
	CreditUsed  int32  `json:"credit_used"`
}

func (a *app) getMe(w http.ResponseWriter, r *http.Request, userID string) {
	ctx := r.Context()
	// auth で user 存在は確認済 (= ここで 401 になるのは race のみ)。
	var user struct {
		Name        string `db:"name"`
		CreditLimit int32  `db:"credit_limit"`
	}
	err := a.db.GetContext(ctx, &user, "SELECT name, credit_limit FROM users WHERE id = ?", userID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusUnauthorized)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// credit_used: 自分が participants にいて current_count < goal_count な campaigns の price 合計。
	var campaignIDs []string
	if err := a.db.SelectContext(ctx, &campaignIDs,
		"SELECT campaign_id FROM campaign_participants WHERE user_id = ?", userID); err != nil {
		writeInternalError(w, err)
		return
	}
	var creditUsed int64
	for _, cid := range campaignIDs {
		var c struct {
			Price     int32 `db:"price"`
			GoalCount int32 `db:"goal_count"`
		}
		if err := a.db.GetContext(ctx, &c, "SELECT price, goal_count FROM campaigns WHERE id = ?", cid); err != nil {
			writeInternalError(w, err)
			return
		}
		var count int64
		if err := a.db.GetContext(ctx, &count,
			"SELECT COUNT(*) FROM campaign_participants WHERE campaign_id = ?", cid); err != nil {
			writeInternalError(w, err)
			return
		}
		if int32(count) < c.GoalCount {
			creditUsed += int64(c.Price)
		}
	}

	writeJSON(w, http.StatusOK, meRes{
		ID:          userID,
		Name:        user.Name,
		CreditLimit: user.CreditLimit,
		CreditUsed:  int32(creditUsed),
	})
}

func (a *app) listTags(w http.ResponseWriter, r *http.Request) {
	names := []string{}
	if err := a.db.SelectContext(r.Context(), &names, "SELECT name FROM tags"); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, names)
}

func (a *app) listCampaigns(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["tags"]) > 1 || len(query["sort"]) > 1 {
		writeError(w, http.StatusBadRequest)
		return
	}

	// tags フィルタ parse
	var tagFilter []string
	if s := query.Get("tags"); s != "" {
		parts := strings.Split(s, ",")
		if len(parts) > 3 {
			writeError(w, http.StatusBadRequest)
			return
		}
		seen := map[string]struct{}{}
		for _, p := range parts {
			if _, dup := seen[p]; dup {
				writeError(w, http.StatusBadRequest)
				return
			}
			seen[p] = struct{}{}
		}
		for _, p := range parts {
			var id string
			err := a.db.GetContext(ctx, &id, "SELECT id FROM tags WHERE name = ?", p)
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusBadRequest)
				return
			}
			if err != nil {
				writeInternalError(w, err)
				return
			}
		}
		tagFilter = parts
	}

	sortMode := "new"
	if v, ok := query["sort"]; ok {
		switch v[0] {
		case "active":
			sortMode = "active"
		case "new":
		default:
			writeError(w, http.StatusBadRequest)
			return
		}
	}

	// 全 campaign 行を hydrate (LIMIT は最後)
	var ids []string
	if err := a.db.SelectContext(ctx, &ids, "SELECT id FROM campaigns"); err != nil {
		writeInternalError(w, err)
		return
	}
	conn, err := a.db.Connx(ctx)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	all := []*campaignRes{}
	for _, cid := range ids {
		c, err := hydrateCampaign(ctx, conn, cid)
		if err != nil {
			conn.Close()
			writeInternalError(w, err)
			return
		}
		if c != nil {
			all = append(all, c)
		}
	}
	conn.Close()

	// フィルタ (status=open + tag AND)
	filtered := []*campaignRes{}
	for _, c := range all {
		if c.Status != "open" {
			continue
		}
		have := map[string]struct{}{}
		for _, t := range c.Tags {
			have[t] = struct{}{}
		}
		matched := true
		for _, want := range tagFilter {
			if _, ok := have[want]; !ok {
				matched = false
				break
			}
		}
		if matched {
			filtered = append(filtered, c)
		}
	}

	// active: COALESCE(last_joined_at, created_at) DESC
	// (last_joined_at は participants 0 人で nil、その場合は created_at で比較)
	if sortMode == "active" {
		key := func(c *campaignRes) time.Time {
			if c.LastJoinedAt != nil {
				return time.Time(*c.LastJoinedAt)
			}
			return time.Time(c.CreatedAt)
		}
		sort.SliceStable(filtered, func(i, j int) bool {
			return key(filtered[i]).After(key(filtered[j]))
		})
	} else {
		sort.SliceStable(filtered, func(i, j int) bool {
			return time.Time(filtered[i].CreatedAt).After(time.Time(filtered[j].CreatedAt))
		})
	}

	if len(filtered) > 30 {
		filtered = filtered[:30]
	}
	writeJSON(w, http.StatusOK, filtered)
}

func (a *app) createCampaign(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()
	m, ok := readJSONObject(r)
	if !ok {
		writeError(w, http.StatusBadRequest)
		return
	}
	name, ok1 := fieldString(m, "name")
	description, ok2 := fieldString(m, "description")
	price, ok3 := fieldInt32(m, "price")
	goalCount, ok4 := fieldInt32(m, "goal_count")
	tags, ok5 := fieldStrings(m, "tags")
	// base64-encoded JPEG (canonical padding required)
	image, ok6 := fieldString(m, "image")
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) {
		writeError(w, http.StatusBadRequest)
		return
	}

	if n := utf8.RuneCountInString(name); n == 0 || n > 100 {
		writeError(w, http.StatusBadRequest)
		return
	}
	if n := utf8.RuneCountInString(description); n == 0 || n > 1000 {
		writeError(w, http.StatusBadRequest)
		return
	}
	if !validatePrice(price) {
		writeError(w, http.StatusBadRequest)
		return
	}
	if goalCount < 2 || goalCount > 20 {
		writeError(w, http.StatusBadRequest)
		return
	}
	if len(tags) > 10 {
		writeError(w, http.StatusBadRequest)
		return
	}
	seen := map[string]struct{}{}
	for _, t := range tags {
		if _, dup := seen[t]; dup {
			writeError(w, http.StatusBadRequest)
			return
		}
		seen[t] = struct{}{}
	}
	imageBytes, status := validateJPEGImageB64(image)
	if status != 0 {
		writeError(w, status)
		return
	}
	tagIDs := []string{}
	for _, t := range tags {
		var tid string
		err := a.db.GetContext(ctx, &tid, "SELECT id FROM tags WHERE name = ?", t)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest)
			return
		}
		if err != nil {
			writeInternalError(w, err)
			return
		}
		for _, id := range tagIDs {
			if id == tid {
				writeError(w, http.StatusBadRequest)
				return
			}
		}
		tagIDs = append(tagIDs, tid)
	}

	id := uuid.NewString()
	now := nowUTC()
	tx, err := a.db.BeginTxx(ctx, nil)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO campaigns (id, name, description, price, goal_count, image, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		id, name, description, price, goalCount, imageBytes, now); err != nil {
		writeInternalError(w, err)
		return
	}
	for _, tid := range tagIDs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO campaign_tags (campaign_id, tag_id, created_at) VALUES (?, ?, ?)",
			id, tid, now); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeInternalError(w, err)
		return
	}

	res, err := a.hydrateCampaignViaPool(ctx, id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if res == nil {
		writeInternalError(w, errors.New("created campaign vanished"))
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// getCampaignImage は毎リクエスト LONGBLOB を引いて SHA256 → ETag を付ける。
// If-None-Match は読まず常に 200 + body を返す。Cache-Control も付けない。
func (a *app) getCampaignImage(w http.ResponseWriter, r *http.Request, _ string) {
	var image []byte
	err := a.db.GetContext(r.Context(), &image, "SELECT image FROM campaigns WHERE id = ?", r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	sum := sha256.Sum256(image)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(image)))
	w.WriteHeader(http.StatusOK)
	w.Write(image)
}

func (a *app) getCampaign(w http.ResponseWriter, r *http.Request, _ string) {
	c, err := a.hydrateCampaignViaPool(r.Context(), r.PathValue("id"))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type webhookCampaign struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Price        int32     `json:"price"`
	GoalCount    int32     `json:"goal_count"`
	CurrentCount int32     `json:"current_count"`
	Tags         []string  `json:"tags"`
	Status       string    `json:"status"`
	CreatedAt    jsonTime  `json:"created_at"`
	LastJoinedAt *jsonTime `json:"last_joined_at"`
}

type webhookPayload struct {
	Type     string          `json:"type"`
	UserID   string          `json:"user_id"`
	Campaign webhookCampaign `json:"campaign"`
}

func (a *app) joinCampaign(w http.ResponseWriter, r *http.Request, userID string) {
	ctx := r.Context()
	campaignID := r.PathValue("id")
	// body は空でも JSON object であること
	if _, ok := readJSONObject(r); !ok {
		writeError(w, http.StatusBadRequest)
		return
	}

	tx, err := a.db.BeginTxx(ctx, nil)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer tx.Rollback()

	// step 1: SELECT user FOR UPDATE (与信会計の serialization)
	//
	// campaign FOR UPDATE 単独だと、同一 user × 別 campaign の並列 join が race して
	// credit_limit を突破できる (両 tx が同じ before_credit_used snapshot を読み合う)。
	// users 行を最初に FOR UPDATE で取ることで「同一 user の credit 操作を直列化」する。
	//
	// REPEATABLE READ の consistent read snapshot は最初の non-locking read で確立する。
	// step 1〜2 は locking read のみで snapshot 未確立。step 3 (COUNT) で snapshot 確立。
	// tx2 が step 1 で blocking → tx1 commit 後に再開 → step 3 の snapshot は tx1 の
	// commit を含む状態になり、step 5.5 の SUM が最新を見ることが保証される。
	var creditLimit int32
	if err := tx.GetContext(ctx, &creditLimit,
		"SELECT credit_limit FROM users WHERE id = ? FOR UPDATE", userID); err != nil {
		writeInternalError(w, err)
		return
	}

	// step 2: SELECT campaign FOR UPDATE (goal_count + price)
	var camp struct {
		GoalCount int32 `db:"goal_count"`
		Price     int32 `db:"price"`
	}
	err = tx.GetContext(ctx, &camp, "SELECT goal_count, price FROM campaigns WHERE id = ? FOR UPDATE", campaignID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	goalCount, price := camp.GoalCount, camp.Price

	// step 3: count
	var before64 int64
	if err := tx.GetContext(ctx, &before64,
		"SELECT COUNT(*) FROM campaign_participants WHERE campaign_id = ?", campaignID); err != nil {
		writeInternalError(w, err)
		return
	}
	before := int32(before64)

	// step 4: closed check before INSERT (= goal_count 超過防止)
	if before >= goalCount {
		writeError(w, http.StatusConflict)
		return
	}

	// step 5: already-joined check
	var dup string
	err = tx.GetContext(ctx, &dup,
		"SELECT id FROM campaign_participants WHERE campaign_id = ? AND user_id = ?", campaignID, userID)
	if err == nil {
		writeError(w, http.StatusConflict)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeInternalError(w, err)
		return
	}

	// step 5.5: credit pre-check (docs/idea.md 仕様)
	//
	// pre-check 固定: 自分自身の participant 行追加 / 自分の close による refund 反映 より前の
	// credit_used に対して判定。step 5 を通過したのでこの user は campaign_id にまだ含まれず、
	// 集計時点で「自分のまだ未追加の participant 行」は除外されている。
	// 最後の 1 人で即 close になるケースでも、close による自分自身の refund を先取りせず判定する。
	var joinedIDs []string
	if err := tx.SelectContext(ctx, &joinedIDs,
		"SELECT campaign_id FROM campaign_participants WHERE user_id = ?", userID); err != nil {
		writeInternalError(w, err)
		return
	}
	var beforeCreditUsed int64
	for _, cid := range joinedIDs {
		var c struct {
			Price     int32 `db:"price"`
			GoalCount int32 `db:"goal_count"`
		}
		if err := tx.GetContext(ctx, &c, "SELECT price, goal_count FROM campaigns WHERE id = ?", cid); err != nil {
			writeInternalError(w, err)
			return
		}
		var count int64
		if err := tx.GetContext(ctx, &count,
			"SELECT COUNT(*) FROM campaign_participants WHERE campaign_id = ?", cid); err != nil {
			writeInternalError(w, err)
			return
		}
		if int32(count) < c.GoalCount {
			beforeCreditUsed += int64(c.Price)
		}
	}
	if int32(beforeCreditUsed)+price > creditLimit {
		writeError(w, http.StatusPaymentRequired)
		return
	}

	// step 6: insert participant
	participantID := uuid.NewString()
	now := nowUTC()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO campaign_participants (id, campaign_id, user_id, created_at) VALUES (?, ?, ?, ?)",
		participantID, campaignID, userID, now); err != nil {
		writeInternalError(w, err)
		return
	}
	after := before + 1

	// step 8: webhook job 収集 (DB だけ、commit 後に送信)
	var webhookUserIDs []string
	if after == goalCount-1 {
		if err := tx.SelectContext(ctx, &webhookUserIDs,
			"SELECT DISTINCT ss.user_id "+
				"FROM saved_searches ss "+
				"JOIN saved_search_tags sst ON sst.saved_search_id = ss.id "+
				"LEFT JOIN campaign_tags ct ON ct.campaign_id = ? AND ct.tag_id = sst.tag_id "+
				"GROUP BY ss.id, ss.user_id "+
				"HAVING COUNT(*) = COUNT(ct.tag_id)",
			campaignID); err != nil {
			writeInternalError(w, err)
			return
		}
	}

	// step 9: closed -> charges INSERT (= 二重課金防止 + 課金漏れ防止)
	if after == goalCount {
		var participantIDs []string
		if err := tx.SelectContext(ctx, &participantIDs,
			"SELECT id FROM campaign_participants WHERE campaign_id = ?", campaignID); err != nil {
			writeInternalError(w, err)
			return
		}
		for _, pid := range participantIDs {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO charges (id, campaign_participant_id, created_at) VALUES (?, ?, ?)",
				uuid.NewString(), pid, now); err != nil {
				writeInternalError(w, err)
				return
			}
		}
	}

	// step 9.5: tx 内で派生計算済みの snapshot を作り、レスポンスと webhook payload の両方に使う。
	// commit 後に hydrate すると並行 join のレースで状態がずれる。
	res, err := hydrateCampaign(ctx, tx, campaignID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if res == nil {
		writeInternalError(w, errors.New("modified campaign vanished"))
		return
	}

	// webhook URL もトランザクション中に取り出しておく (commit 後に DB に触らない)
	webhookURL := ""
	if len(webhookUserIDs) > 0 {
		err := tx.GetContext(ctx, &webhookURL,
			"SELECT value FROM app_config WHERE name = 'notification_webhook_url'")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeInternalError(w, err)
			return
		}
	}

	// step 10: commit (DB 反映が成立してから外部 HTTP)
	if err := tx.Commit(); err != nil {
		writeInternalError(w, err)
		return
	}

	// step 11: webhook 送信 (commit 後、同期・逐次、失敗無視、retry なし)
	// webhook payload は image / hash 系のフィールドを含めない (docs/idea.md)。
	if len(webhookUserIDs) > 0 && webhookURL != "" {
		for _, uid := range webhookUserIDs {
			body, err := json.Marshal(webhookPayload{
				Type:   "campaign_closing_soon",
				UserID: uid,
				Campaign: webhookCampaign{
					ID:           res.ID,
					Name:         res.Name,
					Description:  res.Description,
					Price:        res.Price,
					GoalCount:    res.GoalCount,
					CurrentCount: res.CurrentCount,
					Tags:         res.Tags,
					Status:       res.Status,
					CreatedAt:    res.CreatedAt,
					LastJoinedAt: res.LastJoinedAt,
				},
			})
			if err != nil {
				log.Printf("webhook send to %s for user %s: %v", webhookURL, uid, err)
				continue
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
			if err != nil {
				log.Printf("webhook send to %s for user %s: %v", webhookURL, uid, err)
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := a.http.Do(req)
			if err != nil {
				log.Printf("webhook send to %s for user %s: %v", webhookURL, uid, err)
				continue
			}
			resp.Body.Close()
		}
	}

	writeJSON(w, http.StatusOK, res)
}

func (a *app) createSavedSearch(w http.ResponseWriter, r *http.Request, userID string) {
	ctx := r.Context()
	m, ok := readJSONObject(r)
	if !ok {
		writeError(w, http.StatusBadRequest)
		return
	}
	tags, ok := fieldStrings(m, "tags")
	if !ok {
		writeError(w, http.StatusBadRequest)
		return
	}
	if len(tags) == 0 || len(tags) > 3 {
		writeError(w, http.StatusBadRequest)
		return
	}
	seen := map[string]struct{}{}
	for _, t := range tags {
		if _, dup := seen[t]; dup {
			writeError(w, http.StatusBadRequest)
			return
		}
		seen[t] = struct{}{}
	}
	tagIDs := []string{}
	for _, t := range tags {
		var tid string
		err := a.db.GetContext(ctx, &tid, "SELECT id FROM tags WHERE name = ?", t)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest)
			return
		}
		if err != nil {
			writeInternalError(w, err)
			return
		}
		for _, id := range tagIDs {
			if id == tid {
				writeError(w, http.StatusBadRequest)
				return
			}
		}
		tagIDs = append(tagIDs, tid)
	}

	// 件数上限 10 を「ユーザ行 FOR UPDATE → COUNT → INSERT」を 1 トランザクションで実行し、
	// 同一ユーザの並行 POST が両方 10 未満を観測して両方 INSERT してしまう競合を防ぐ。
	tx, err := a.db.BeginTxx(ctx, nil)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer tx.Rollback()
	var locked string
	if err := tx.GetContext(ctx, &locked, "SELECT id FROM users WHERE id = ? FOR UPDATE", userID); err != nil {
		writeInternalError(w, err)
		return
	}
	var count int64
	if err := tx.GetContext(ctx, &count, "SELECT COUNT(*) FROM saved_searches WHERE user_id = ?", userID); err != nil {
		writeInternalError(w, err)
		return
	}
	if count >= 10 {
		writeError(w, http.StatusConflict)
		return
	}

	ssID := uuid.NewString()
	now := nowUTC()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO saved_searches (id, user_id, created_at) VALUES (?, ?, ?)",
		ssID, userID, now); err != nil {
		writeInternalError(w, err)
		return
	}
	for _, tid := range tagIDs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO saved_search_tags (saved_search_id, tag_id, created_at) VALUES (?, ?, ?)",
			ssID, tid, now); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeInternalError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

type chargeRes struct {
	ID        string         `json:"id"`
	Amount    int32          `json:"amount"`
	Campaign  chargeCampaign `json:"campaign"`
	CreatedAt jsonTime       `json:"created_at"`
}

type chargeCampaign struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int32  `json:"price"`
}

func (a *app) listCharges(w http.ResponseWriter, r *http.Request, userID string) {
	rows, err := a.db.QueryContext(r.Context(),
		"SELECT ch.id, ch.created_at, c.id, c.name, c.price "+
			"FROM charges ch "+
			"JOIN campaign_participants cp ON ch.campaign_participant_id = cp.id "+
			"JOIN campaigns c ON cp.campaign_id = c.id "+
			"WHERE cp.user_id = ? "+
			"ORDER BY ch.created_at DESC",
		userID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	defer rows.Close()
	res := []chargeRes{}
	for rows.Next() {
		var (
			id, cid, name string
			createdAt     time.Time
			price         int32
		)
		if err := rows.Scan(&id, &createdAt, &cid, &name, &price); err != nil {
			writeInternalError(w, err)
			return
		}
		res = append(res, chargeRes{
			ID:        id,
			Amount:    price,
			Campaign:  chargeCampaign{ID: cid, Name: name, Price: price},
			CreatedAt: jsonTime(createdAt),
		})
	}
	if err := rows.Err(); err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// === campaign hydration (派生計算: current_count / status / last_joined_at / tags / participants) ===

// hydrateCampaign は campaign とその派生値を q 上で組み立てる。存在しなければ (nil, nil)。
// トランザクション内から呼べば同一 snapshot 上で計算できる (= concurrent join とのレース無し)。
func hydrateCampaign(ctx context.Context, q sqlx.QueryerContext, id string) (*campaignRes, error) {
	// image LONGBLOB は引かない。画像バイナリは GET /api/campaigns/{id}/image で個別に返す。
	var row struct {
		ID          string    `db:"id"`
		Name        string    `db:"name"`
		Description string    `db:"description"`
		Price       int32     `db:"price"`
		GoalCount   int32     `db:"goal_count"`
		CreatedAt   time.Time `db:"created_at"`
	}
	err := sqlx.GetContext(ctx, q, &row,
		"SELECT id, name, description, price, goal_count, created_at FROM campaigns WHERE id = ?", id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	tags := []string{}
	if err := sqlx.SelectContext(ctx, q, &tags,
		"SELECT t.name FROM campaign_tags ct JOIN tags t ON ct.tag_id = t.id WHERE ct.campaign_id = ?",
		row.ID); err != nil {
		return nil, err
	}

	var partRows []struct {
		UserID    string    `db:"user_id"`
		Name      string    `db:"name"`
		CreatedAt time.Time `db:"created_at"`
	}
	if err := sqlx.SelectContext(ctx, q, &partRows,
		"SELECT cp.user_id, u.name, cp.created_at "+
			"FROM campaign_participants cp JOIN users u ON cp.user_id = u.id "+
			"WHERE cp.campaign_id = ? ORDER BY cp.created_at ASC",
		row.ID); err != nil {
		return nil, err
	}
	participants := make([]participantRes, 0, len(partRows))
	for _, p := range partRows {
		participants = append(participants, participantRes{
			UserID:   p.UserID,
			Name:     p.Name,
			JoinedAt: jsonTime(p.CreatedAt),
		})
	}

	currentCount := int32(len(participants))
	var lastJoinedAt *jsonTime
	if len(participants) > 0 {
		t := participants[len(participants)-1].JoinedAt
		lastJoinedAt = &t
	}
	status := "open"
	if currentCount >= row.GoalCount {
		status = "closed"
	}

	return &campaignRes{
		ID:           row.ID,
		Name:         row.Name,
		Description:  row.Description,
		Price:        row.Price,
		GoalCount:    row.GoalCount,
		CurrentCount: currentCount,
		Tags:         tags,
		Status:       status,
		CreatedAt:    jsonTime(row.CreatedAt),
		LastJoinedAt: lastJoinedAt,
		Participants: participants,
	}, nil
}

// hydrateCampaignViaPool は pool から 1 接続を確保して hydrateCampaign を実行する。
func (a *app) hydrateCampaignViaPool(ctx context.Context, id string) (*campaignRes, error) {
	conn, err := a.db.Connx(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return hydrateCampaign(ctx, conn, id)
}
